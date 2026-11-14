package mattermost

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/multica-ai/multica/server/internal/events"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/protocol"
)

type mentionTestQueries struct {
	installation db.ChannelInstallation
	members      []db.ListMembersWithUserRow
}

func (*mentionTestQueries) GetWorkspace(context.Context, pgtype.UUID) (db.Workspace, error) {
	return db.Workspace{Slug: "engineering"}, nil
}

func (q *mentionTestQueries) GetChannelInstallation(_ context.Context, arg db.GetChannelInstallationParams) (db.ChannelInstallation, error) {
	if arg.ID != q.installation.ID || arg.ChannelType != string(TypeMattermost) {
		panic("unexpected installation lookup")
	}
	return q.installation, nil
}

func (q *mentionTestQueries) ListMembersWithUser(context.Context, pgtype.UUID) ([]db.ListMembersWithUserRow, error) {
	return q.members, nil
}

func TestMentionNotifications(t *testing.T) {
	workspace := util.MustParseUUID("10000000-0000-0000-0000-000000000001")
	recipient := util.MustParseUUID("10000000-0000-0000-0000-000000000002")
	installation := util.MustParseUUID("10000000-0000-0000-0000-000000000003")
	issue := "10000000-0000-0000-0000-000000000004"

	for _, tc := range []struct {
		name        string
		kind        string
		disabled    bool
		broadcast   bool
		wrongSpace  bool
		duplicate   bool
		missingUser bool
	}{
		{name: "mention", kind: "mentioned"},
		{name: "unrelated notification", kind: "assigned"},
		{name: "broadcast mention", kind: "mentioned", broadcast: true},
		{name: "disabled workspace", kind: "mentioned", disabled: true},
		{name: "bot in another workspace", kind: "mentioned", wrongSpace: true},
		{name: "duplicate profile name", kind: "mentioned", duplicate: true},
		{name: "Mattermost account missing", kind: "mentioned", missingUser: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			requests := make(chan string, 10)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests <- r.Method + " " + r.URL.Path
				if r.Header.Get("Authorization") != "Bearer test-token" {
					t.Error("unexpected bot credentials")
				}
				switch r.URL.Path {
				case "/api/v4/users/username/alice":
					if tc.missingUser {
						w.WriteHeader(http.StatusNotFound)
						return
					}
					_ = json.NewEncoder(w).Encode(User{ID: "alice-id", Username: "alice"})
				case "/api/v4/channels/direct":
					var ids []string
					if err := json.NewDecoder(r.Body).Decode(&ids); err != nil || !slices.Equal(ids, []string{"bot-id", "alice-id"}) {
						t.Errorf("unexpected direct channel participants: %v (%v)", ids, err)
					}
					_, _ = w.Write([]byte(`{"id":"dm-id"}`))
				case "/api/v4/posts":
					var post Post
					if err := json.NewDecoder(r.Body).Decode(&post); err != nil || post.ChannelID != "dm-id" ||
						!strings.Contains(post.Message, "https://multica.example/engineering/issues/"+issue) ||
						strings.Contains(post.Message, "private title") {
						t.Errorf("unexpected mention post: %+v (%v)", post, err)
					}
					_, _ = w.Write([]byte(`{"id":"post-id"}`))
				default:
					t.Errorf("unexpected request: %s", r.URL.Path)
					w.WriteHeader(http.StatusNotFound)
				}
			}))
			defer server.Close()

			config, err := json.Marshal(installConfig{
				ServerURL: server.URL, BotUserID: "bot-id",
				AccessTokenEncrypted: base64.StdEncoding.EncodeToString([]byte("test-token")),
			})
			if err != nil {
				t.Fatal(err)
			}
			q := &mentionTestQueries{
				installation: db.ChannelInstallation{ID: installation, WorkspaceID: workspace, Status: "active", Config: config},
				members:      []db.ListMembersWithUserRow{{UserID: recipient, UserName: "Alice"}},
			}
			if tc.wrongSpace {
				q.installation.WorkspaceID = installation
			}
			if tc.duplicate {
				q.members = append(q.members, db.ListMembersWithUserRow{UserID: installation, UserName: "alice"})
			}
			routes := "engineering=" + util.UUIDToString(installation)
			if tc.disabled {
				routes = "another-workspace=" + util.UUIDToString(installation)
			}
			bus := events.New()
			RegisterMentionNotifications(bus, q, nil, "https://multica.example", routes)
			bus.Publish(events.Event{
				Type: protocol.EventInboxNew, WorkspaceID: util.UUIDToString(workspace), DirectMemberMention: !tc.broadcast,
				Payload: map[string]any{"item": map[string]any{
					"type": tc.kind, "recipient_type": "member", "recipient_id": util.UUIDToString(recipient),
					"read": false, "issue_id": &issue, "title": "private title",
				}},
			})

			close(requests)
			var got []string
			for request := range requests {
				got = append(got, request)
			}
			var want []string
			switch tc.name {
			case "mention":
				want = []string{"GET /api/v4/users/username/alice", "POST /api/v4/channels/direct", "POST /api/v4/posts"}
			case "Mattermost account missing":
				want = []string{"GET /api/v4/users/username/alice"}
			}
			if !slices.Equal(got, want) {
				t.Errorf("requests = %v, want %v", got, want)
			}
		})
	}
}
