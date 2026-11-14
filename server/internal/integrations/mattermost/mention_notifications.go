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
	GetChannelInstallation(context.Context, db.GetChannelInstallationParams) (db.ChannelInstallation, error)
	ListMembersWithUser(context.Context, pgtype.UUID) ([]db.ListMembersWithUserRow, error)
}

type mentionNotifications struct {
	queries       mentionQueries
	decrypt       Decrypter
	appURL        string
	installations map[string]pgtype.UUID
}

var mattermostUsername = regexp.MustCompile(`^[a-z][a-z0-9._-]{2,21}$`)

// RegisterMentionNotifications accepts comma-separated workspace-slug=installation-UUID pairs.
func RegisterMentionNotifications(bus *events.Bus, queries mentionQueries, decrypt Decrypter, appURL, routes string) {
	if strings.TrimSpace(routes) == "" {
		return
	}
	notifications := mentionNotifications{
		queries: queries, decrypt: decrypt, appURL: appURL,
		installations: make(map[string]pgtype.UUID),
	}
	for _, route := range strings.Split(routes, ",") {
		slug, rawID, ok := strings.Cut(strings.TrimSpace(route), "=")
		slug = strings.TrimSpace(slug)
		id, err := util.ParseUUID(strings.TrimSpace(rawID))
		if !ok || slug == "" || err != nil || notifications.installations[slug].Valid {
			slog.Error("mattermost: invalid MULTICA_MATTERMOST_MENTION_BOTS configuration")
			return
		}
		notifications.installations[slug] = id
	}

	bus.Subscribe(protocol.EventInboxNew, notifications.handle)
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
	workspace, err := n.queries.GetWorkspace(ctx, workspaceID)
	if err != nil {
		return err
	}
	installationID, enabled := n.installations[workspace.Slug]
	if !enabled {
		return nil
	}
	installation, err := n.queries.GetChannelInstallation(ctx, db.GetChannelInstallationParams{
		ID: installationID, ChannelType: string(TypeMattermost),
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	if installation.WorkspaceID != workspaceID || installation.Status != "active" {
		return nil
	}

	members, err := n.queries.ListMembersWithUser(ctx, workspaceID)
	if err != nil {
		return err
	}
	username := mentionUsername(members, recipientID)
	if username == "" {
		return nil
	}
	creds, err := decodeCredentials(installation.Config, n.decrypt)
	if err != nil {
		return err
	}
	client := newRESTClient(creds.ServerURL, creds.AccessToken, nil)
	user, err := client.getUserByUsername(ctx, username)
	if statusOf(err) == http.StatusNotFound {
		return nil
	}
	if err != nil {
		return err
	}
	if user.ID == "" || user.ID == creds.BotUserID || user.IsBot || user.DeleteAt != 0 || user.Username != username {
		return nil
	}

	channelID, err := client.createDirectChannel(ctx, creds.BotUserID, user.ID)
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

func mentionUsername(members []db.ListMembersWithUserRow, recipientID pgtype.UUID) string {
	username := ""
	for _, member := range members {
		if member.UserID == recipientID {
			username = strings.ToLower(strings.TrimSpace(member.UserName))
			break
		}
	}
	if !mattermostUsername.MatchString(username) {
		return ""
	}
	for _, member := range members {
		if member.UserID != recipientID && strings.EqualFold(strings.TrimSpace(member.UserName), username) {
			return ""
		}
	}
	return username
}
