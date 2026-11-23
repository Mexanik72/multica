package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/multica-ai/multica/server/internal/analytics"
	"github.com/multica-ai/multica/server/internal/inbox"
	"github.com/multica-ai/multica/server/internal/integrations/mattermost"
	"github.com/multica-ai/multica/server/internal/realtime"
	"github.com/multica-ai/multica/server/internal/testutil"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

func TestIssueCommentMentionSendsMattermostDM(t *testing.T) {
	const (
		username      = "mm-comment-recipient"
		workspaceSlug = "mm-comment-notifications"
		appURL        = "https://multica.example"
		botToken      = "test-bot-token"
	)
	fixture := testutil.New(testPool, testWorkspaceID, testUserID)
	workspace := fixture.Workspace(t, "Mattermost comment notifications", workspaceSlug)
	recipient := fixture.User(t, "MM-Comment-Recipient", "mm-comment-recipient@example.test")
	fixture.Member(t, workspace, testUserID, "owner")
	fixture.Member(t, workspace, recipient, "member")
	issue := fixture.Issue(t, "Test Mattermost DM", testutil.Cols{"workspace_id": workspace})
	t.Cleanup(func() { cleanupInboxForIssue(t, issue) })

	posts := make(chan mattermost.Post, 1)
	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+botToken {
			http.Error(w, "invalid token", http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		switch r.Method + " " + r.URL.Path {
		case "GET /api/v4/users/username/" + username:
			io.WriteString(w, `{"id":"recipient-id","username":"`+username+`"}`)
		case "GET /api/v4/users/me":
			io.WriteString(w, `{"id":"bot-id","username":"multica-bot","is_bot":true}`)
		case "POST /api/v4/channels/direct":
			var members []string
			err := json.NewDecoder(r.Body).Decode(&members)
			slices.Sort(members)
			if err != nil || !slices.Equal(members, []string{"bot-id", "recipient-id"}) {
				t.Errorf("direct channel members = %v, decode error = %v", members, err)
				http.Error(w, "unexpected channel members", http.StatusBadRequest)
				return
			}
			io.WriteString(w, `{"id":"direct-channel-id"}`)
		case "POST /api/v4/posts":
			var post mattermost.Post
			if err := json.NewDecoder(r.Body).Decode(&post); err != nil {
				t.Errorf("decode Mattermost post: %v", err)
				http.Error(w, "invalid post", http.StatusBadRequest)
				return
			}
			select {
			case posts <- post:
			default:
				t.Error("unexpected duplicate Mattermost post")
			}
			w.WriteHeader(http.StatusCreated)
			io.WriteString(w, `{"id":"post-id"}`)
		default:
			t.Errorf("unexpected Mattermost request: %s %s", r.Method, r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(mock.Close)

	t.Setenv("MULTICA_APP_URL", appURL)
	t.Setenv("MULTICA_MATTERMOST_NOTIFICATION_URL", mock.URL)
	t.Setenv("MULTICA_MATTERMOST_NOTIFICATION_TOKEN", botToken)
	queries := db.New(testPool)
	bus := newNotificationBus(t, queries)
	router := NewRouter(testPool, realtime.NewHub(), bus, analytics.NoopClient{}, nil)
	req := testutil.JSONRequest(http.MethodPost, "/api/issues/"+issue+"/comments", map[string]string{
		"content": "Could you take a look, [@MM-Comment-Recipient](mention://member/" + recipient + ")?",
	})
	req = testutil.WithHeaders(req, "Authorization", "Bearer "+testToken, "X-Workspace-ID", workspace)
	testutil.Call(t, router.ServeHTTP, req).Want(http.StatusCreated)

	items, err := queries.ListInboxItems(context.Background(), db.ListInboxItemsParams{
		WorkspaceID:   util.MustParseUUID(workspace),
		RecipientType: "member",
		RecipientID:   util.MustParseUUID(recipient),
		RowLimit:      inbox.ActiveLimit,
	})
	if err != nil {
		t.Fatal(err)
	}
	issueID := util.MustParseUUID(issue)
	if !slices.ContainsFunc(items, func(item db.ListInboxItemsRow) bool {
		return item.Type == "mentioned" && item.IssueID == issueID
	}) {
		t.Fatal("comment mention did not appear in the recipient's Multica inbox")
	}
	select {
	case post := <-posts:
		issueURL := appURL + "/" + workspaceSlug + "/issues/" + issue
		if post.ChannelID != "direct-channel-id" || !strings.Contains(post.Message, issueURL) {
			t.Errorf("Mattermost post = %+v, want DM with link %s", post, issueURL)
		}
	default:
		t.Fatal("comment mention appeared in Multica but no Mattermost DM was sent")
	}
}

func TestMattermostMentionUsernameIdentifiesDuplicateWorkspaceMemberName(t *testing.T) {
	ctx := context.Background()
	fixture := testutil.New(testPool, testWorkspaceID, testUserID)
	workspace := fixture.Workspace(t, "Mattermost notifications", "mm-notification-username")
	otherWorkspace := fixture.Workspace(t, "Other workspace", "mm-notification-other")
	user := fixture.User(t, "  MM-Notice-Fixture\t", "mm-notification-username@example.test")
	fixture.Member(t, workspace, user, "member")

	q := db.New(testPool)
	params := db.GetMattermostMentionUsernameParams{
		WorkspaceID: util.MustParseUUID(workspace),
		UserID:      util.MustParseUUID(user),
	}
	if profile, err := q.GetMattermostMentionUsername(ctx, params); err != nil || profile.Username != "mm-notice-fixture" || profile.DuplicateName {
		t.Fatalf("unique workspace member: profile = %+v, err = %v", profile, err)
	}
	foreignParams := params
	foreignParams.WorkspaceID = util.MustParseUUID(otherWorkspace)
	if _, err := q.GetMattermostMentionUsername(ctx, foreignParams); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("user outside workspace: got %v, want ErrNoRows", err)
	}

	duplicate := fixture.User(t, "mm-notice-fixture", "mm-notification-duplicate@example.test")
	fixture.Member(t, otherWorkspace, duplicate, "member")
	if profile, err := q.GetMattermostMentionUsername(ctx, params); err != nil || !profile.DuplicateName {
		t.Fatalf("duplicate name in another workspace: profile = %+v, err = %v", profile, err)
	}
}
