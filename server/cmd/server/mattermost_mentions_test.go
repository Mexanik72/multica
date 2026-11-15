package main

import (
	"context"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/multica-ai/multica/server/internal/testutil"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

func TestMattermostMentionUsernameRequiresUniqueWorkspaceMember(t *testing.T) {
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
	if name, err := q.GetMattermostMentionUsername(ctx, params); err != nil || name != "mm-notice-fixture" {
		t.Fatalf("unique workspace member: username = %q, err = %v", name, err)
	}
	foreignParams := params
	foreignParams.WorkspaceID = util.MustParseUUID(otherWorkspace)
	if _, err := q.GetMattermostMentionUsername(ctx, foreignParams); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("user outside workspace: got %v, want ErrNoRows", err)
	}

	duplicate := fixture.User(t, "mm-notice-fixture", "mm-notification-duplicate@example.test")
	fixture.Member(t, otherWorkspace, duplicate, "member")
	if _, err := q.GetMattermostMentionUsername(ctx, params); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("duplicate name in another workspace: got %v, want ErrNoRows", err)
	}
}
