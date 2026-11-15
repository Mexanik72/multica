package mattermost

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/multica-ai/multica/server/internal/events"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/protocol"
)

type mentionTestQueries struct {
	workspaces map[pgtype.UUID]string
	recipient  pgtype.UUID
	profile    db.GetMattermostMentionUsernameRow
}

func (q mentionTestQueries) GetWorkspace(_ context.Context, id pgtype.UUID) (db.Workspace, error) {
	slug, exists := q.workspaces[id]
	if !exists {
		return db.Workspace{}, pgx.ErrNoRows
	}
	return db.Workspace{Slug: slug}, nil
}

func (q mentionTestQueries) GetMattermostMentionUsername(_ context.Context, arg db.GetMattermostMentionUsernameParams) (db.GetMattermostMentionUsernameRow, error) {
	if _, exists := q.workspaces[arg.WorkspaceID]; !exists || arg.UserID != q.recipient {
		return db.GetMattermostMentionUsernameRow{}, pgx.ErrNoRows
	}
	return q.profile, nil
}

func TestMentionNotifications(t *testing.T) {
	workspace := util.MustParseUUID("10000000-0000-0000-0000-000000000001")
	secondWorkspace := util.MustParseUUID("10000000-0000-0000-0000-000000000003")
	recipient := util.MustParseUUID("10000000-0000-0000-0000-000000000002")
	issue := "10000000-0000-0000-0000-000000000004"

	for _, tc := range []struct {
		name          string
		kind          string
		disabled      bool
		broadcast     bool
		notEligible   bool
		duplicateName bool
		missingUser   bool
		invalidName   bool
		inactiveUser  bool
		missingIssue  bool
		personalToken bool
		wantReason    string
	}{
		{name: "mention", kind: "mentioned"},
		{name: "unrelated notification", kind: "assigned"},
		{name: "broadcast mention", kind: "mentioned", broadcast: true},
		{name: "disabled server", kind: "mentioned", disabled: true, wantReason: "notification_bot_not_configured"},
		{name: "ineligible account", kind: "mentioned", notEligible: true, wantReason: "recipient_not_workspace_member"},
		{name: "duplicate profile name", kind: "mentioned", duplicateName: true, wantReason: "duplicate_multica_profile_name"},
		{name: "invalid profile name", kind: "mentioned", invalidName: true, wantReason: "invalid_multica_profile_name"},
		{name: "missing issue ID", kind: "mentioned", missingIssue: true, wantReason: "invalid_inbox_event"},
		{name: "personal token", kind: "mentioned", personalToken: true},
		{name: "Mattermost account missing", kind: "mentioned", missingUser: true, wantReason: "mattermost_user_not_found"},
		{name: "Mattermost account deactivated", kind: "mentioned", inactiveUser: true, wantReason: "mattermost_user_deactivated"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var logs bytes.Buffer
			previous := slog.Default()
			slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
			t.Cleanup(func() { slog.SetDefault(previous) })

			requests := make(chan string, 10)
			links := make(chan string, 2)
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
					user := User{ID: "alice-id", Username: "alice"}
					if tc.inactiveUser {
						user.DeleteAt = 1
					}
					_ = json.NewEncoder(w).Encode(user)
				case "/api/v4/users/me":
					_ = json.NewEncoder(w).Encode(User{ID: "bot-id", IsBot: !tc.personalToken})
				case "/api/v4/channels/direct":
					var ids []string
					if err := json.NewDecoder(r.Body).Decode(&ids); err != nil || !slices.Equal(ids, []string{"bot-id", "alice-id"}) {
						t.Errorf("unexpected direct channel participants: %v (%v)", ids, err)
					}
					_, _ = w.Write([]byte(`{"id":"dm-id"}`))
				case "/api/v4/posts":
					var post Post
					if err := json.NewDecoder(r.Body).Decode(&post); err != nil || post.ChannelID != "dm-id" ||
						!strings.HasSuffix(post.Message, "/issues/"+issue) ||
						strings.Contains(post.Message, "private title") {
						t.Errorf("unexpected mention post: %+v (%v)", post, err)
					}
					links <- post.Message
					_, _ = w.Write([]byte(`{"id":"post-id"}`))
				default:
					t.Errorf("unexpected request: %s", r.URL.Path)
					w.WriteHeader(http.StatusNotFound)
				}
			}))
			defer server.Close()

			q := mentionTestQueries{
				workspaces: map[pgtype.UUID]string{workspace: "engineering", secondWorkspace: "support"},
				recipient:  recipient,
				profile:    db.GetMattermostMentionUsernameRow{Username: "alice", DuplicateName: tc.duplicateName},
			}
			if tc.notEligible {
				q.recipient = pgtype.UUID{}
			}
			if tc.invalidName {
				q.profile.Username = "alice example"
			}
			serverURL, token := server.URL, "test-token"
			if tc.disabled {
				serverURL, token = "", ""
			}
			bus := events.New()
			if err := RegisterMentionNotifications(bus, q, "https://multica.example", serverURL, token); err != nil {
				t.Fatal(err)
			}
			item := map[string]any{
				"type": tc.kind, "recipient_type": "member", "recipient_id": util.UUIDToString(recipient),
				"read": false, "issue_id": &issue, "title": "private title",
			}
			if tc.missingIssue {
				delete(item, "issue_id")
			}
			event := events.Event{
				Type: protocol.EventInboxNew, WorkspaceID: util.UUIDToString(workspace),
				DirectMemberMention: !tc.broadcast && tc.kind == "mentioned",
				Payload:             map[string]any{"item": item},
			}
			bus.Publish(event)
			if tc.name == "mention" {
				event.WorkspaceID = util.UUIDToString(secondWorkspace)
				bus.Publish(event)
			}

			close(requests)
			close(links)
			var got []string
			for request := range requests {
				got = append(got, request)
			}
			var want []string
			switch tc.name {
			case "mention":
				requestsPerMention := []string{"GET /api/v4/users/username/alice", "GET /api/v4/users/me", "POST /api/v4/channels/direct", "POST /api/v4/posts"}
				want = append(slices.Clone(requestsPerMention), requestsPerMention...)
				for _, slug := range []string{"engineering", "support"} {
					if message := <-links; !strings.Contains(message, "https://multica.example/"+slug+"/issues/"+issue) {
						t.Errorf("missing link for %s in %q", slug, message)
					}
				}
			case "personal token":
				want = []string{"GET /api/v4/users/username/alice", "GET /api/v4/users/me"}
			case "Mattermost account missing", "Mattermost account deactivated":
				want = []string{"GET /api/v4/users/username/alice"}
			}
			if !slices.Equal(got, want) {
				t.Errorf("requests = %v, want %v", got, want)
			}
			if tc.wantReason != "" && !strings.Contains(logs.String(), "reason="+tc.wantReason) {
				t.Errorf("missing reason %q in logs: %s", tc.wantReason, &logs)
			}
			if strings.Contains(logs.String(), "test-token") || strings.Contains(logs.String(), "private title") {
				t.Error("mention logs contain credentials or private issue content")
			}
		})
	}
}
