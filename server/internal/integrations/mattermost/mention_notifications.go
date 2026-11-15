package mattermost

import (
	"context"
	"errors"
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
	GetMattermostMentionUsername(context.Context, db.GetMattermostMentionUsernameParams) (string, error)
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
	payload, _ := event.Payload.(map[string]any)
	item, _ := payload["item"].(map[string]any)
	if !event.DirectMemberMention || item["type"] != "mentioned" || item["recipient_type"] != "member" || item["read"] != false {
		return
	}
	rawRecipient, _ := item["recipient_id"].(string)
	recipient, recipientErr := util.ParseUUID(rawRecipient)
	workspace, workspaceErr := util.ParseUUID(event.WorkspaceID)
	rawIssue, _ := item["issue_id"].(*string)
	if recipientErr != nil || workspaceErr != nil || rawIssue == nil {
		return
	}
	issue, err := util.ParseUUID(*rawIssue)
	if err != nil {
		return
	}

	// The event bus is synchronous. Bound the time spent on external delivery
	// and keep failures independent of inbox delivery.
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := n.deliver(ctx, workspace, recipient, issue); err != nil {
		slog.WarnContext(ctx, "mattermost: mention notification failed", "workspace_id", event.WorkspaceID, "error", err)
	}
}

func (n mentionNotifications) deliver(ctx context.Context, workspaceID, recipientID, issueID pgtype.UUID) error {
	username, err := n.queries.GetMattermostMentionUsername(ctx, db.GetMattermostMentionUsernameParams{
		WorkspaceID: workspaceID, UserID: recipientID,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	if !mattermostUsername.MatchString(username) {
		return nil
	}

	workspace, err := n.queries.GetWorkspace(ctx, workspaceID)
	if err != nil {
		return err
	}
	client := n.client
	user, err := client.getUserByUsername(ctx, username)
	if statusOf(err) == http.StatusNotFound {
		return nil
	}
	if err != nil {
		return err
	}
	if user.ID == "" || user.IsBot || user.DeleteAt != 0 || user.Username != username {
		return nil
	}

	bot, err := client.GetMe(ctx)
	if err != nil {
		return err
	}
	if bot.ID == "" || !bot.IsBot || bot.DeleteAt != 0 || bot.ID == user.ID {
		return ErrNotABotAccount
	}

	channelID, err := client.createDirectChannel(ctx, bot.ID, user.ID)
	if err != nil {
		return err
	}
	if channelID == "" {
		return errors.New("mattermost: empty direct channel ID")
	}
	message := "You were mentioned in Multica."
	if n.appURL != "" {
		message += "\n" + n.appURL + "/" + url.PathEscape(workspace.Slug) + "/issues/" + util.UUIDToString(issueID)
	}
	_, err = client.CreatePost(ctx, Post{ChannelID: channelID, Message: message})
	return err
}
