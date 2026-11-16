package mattermost

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/multica-ai/multica/server/internal/events"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/protocol"
)

type mentionQueries interface {
	GetWorkspace(context.Context, pgtype.UUID) (db.Workspace, error)
	GetMattermostMentionUsername(context.Context, db.GetMattermostMentionUsernameParams) (db.GetMattermostMentionUsernameRow, error)
}

type mentionNotifications struct {
	queries mentionQueries
	client  *restClient
	appURL  string
}

var mattermostUsername = regexp.MustCompile(`^[a-z][a-z0-9._-]{2,21}$`)

// RegisterMentionNotifications registers the server-wide bot when its URL and
// token are configured. The bot does not need a channel installation.
func RegisterMentionNotifications(bus *events.Bus, queries mentionQueries, appURL, serverURL, token string) error {
	if strings.TrimSpace(serverURL) == "" && strings.TrimSpace(token) == "" {
		slog.Info("mattermost mention notifications disabled", "reason", "notification_bot_not_configured")
		return nil
	}
	serverURL, err := canonicalServerURL(serverURL)
	if err != nil {
		return ErrInvalidServerURL
	}
	token, err = validateAccessToken(token)
	if err != nil {
		return err
	}
	notifications := mentionNotifications{
		queries: queries, client: newRESTClient(serverURL, token, nil), appURL: appURL,
	}
	bus.Subscribe(protocol.EventInboxNew, notifications.handle)
	return nil
}

func (n mentionNotifications) handle(event events.Event) {
	if !event.DirectMemberMention {
		return
	}
	logger := slog.With("workspace_id", event.WorkspaceID)
	payload, _ := event.Payload.(map[string]any)
	item, _ := payload["item"].(map[string]any)
	rawRecipient, _ := item["recipient_id"].(string)
	recipient, recipientErr := util.ParseUUID(rawRecipient)
	workspace, workspaceErr := util.ParseUUID(event.WorkspaceID)
	rawIssue, _ := item["issue_id"].(*string)
	if item["type"] != "mentioned" || item["recipient_type"] != "member" || item["read"] != false ||
		recipientErr != nil || workspaceErr != nil || rawIssue == nil {
		logger.Warn("mattermost: mention notification skipped", "reason", "invalid_inbox_event")
		return
	}
	issue, err := util.ParseUUID(*rawIssue)
	if err != nil {
		logger.Warn("mattermost: mention notification skipped", "reason", "invalid_inbox_event")
		return
	}
	logger = logger.With("recipient_id", rawRecipient, "issue_id", *rawIssue)

	// The event bus is synchronous. Bound the time spent on external delivery
	// and keep failures independent of inbox delivery.
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	reason, err := n.deliver(ctx, workspace, recipient, issue)
	if err != nil {
		logger.WarnContext(ctx, "mattermost: mention notification failed", "error", err)
	} else if reason != "" {
		logger.InfoContext(ctx, "mattermost: mention notification skipped", "reason", reason)
	}
}

func (n mentionNotifications) deliver(ctx context.Context, workspaceID, recipientID, issueID pgtype.UUID) (skipReason string, err error) {
	recipient, err := n.queries.GetMattermostMentionUsername(ctx, db.GetMattermostMentionUsernameParams{
		WorkspaceID: workspaceID, UserID: recipientID,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return "recipient_not_workspace_member", nil
	}
	if err != nil {
		return "", fmt.Errorf("look up Multica recipient: %w", err)
	}
	if recipient.DuplicateName {
		return "duplicate_multica_profile_name", nil
	}
	username := recipient.Username
	if !mattermostUsername.MatchString(username) {
		return "invalid_multica_profile_name", nil
	}

	workspace, err := n.queries.GetWorkspace(ctx, workspaceID)
	if err != nil {
		return "", fmt.Errorf("load Multica workspace: %w", err)
	}
	client := n.client
	user, err := client.getUserByUsername(ctx, username)
	if statusOf(err) == http.StatusNotFound {
		return "mattermost_user_not_found", nil
	}
	if err != nil {
		return "", fmt.Errorf("look up Mattermost recipient: %w", err)
	}
	if user.ID == "" || user.Username != username {
		return "invalid_mattermost_recipient", nil
	}
	if user.IsBot {
		return "mattermost_recipient_is_bot", nil
	}
	if user.DeleteAt != 0 {
		return "mattermost_user_deactivated", nil
	}

	bot, err := client.GetMe(ctx)
	if err != nil {
		return "", fmt.Errorf("verify Mattermost notification bot: %w", err)
	}
	if bot.ID == "" || !bot.IsBot || bot.DeleteAt != 0 || bot.ID == user.ID {
		return "", ErrNotABotAccount
	}

	channelID, err := client.createDirectChannel(ctx, bot.ID, user.ID)
	if err != nil {
		return "", fmt.Errorf("create Mattermost direct channel: %w", err)
	}
	if channelID == "" {
		return "", errors.New("mattermost: empty direct channel ID")
	}
	message := "You were mentioned in Multica."
	if n.appURL != "" {
		message += "\n" + n.appURL + "/" + url.PathEscape(workspace.Slug) + "/issues/" + util.UUIDToString(issueID)
	}
	if _, err := client.CreatePost(ctx, Post{ChannelID: channelID, Message: message}); err != nil {
		return "", fmt.Errorf("send Mattermost direct message: %w", err)
	}
	return "", nil
}
