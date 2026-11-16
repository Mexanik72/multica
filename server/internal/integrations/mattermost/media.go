package mattermost

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"net/url"
	"path"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/multica-ai/multica/server/internal/integrations/channel"
	"github.com/multica-ai/multica/server/internal/integrations/channel/engine"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

const (
	maxInboundFiles     = 10
	maxInboundFileBytes = 20 << 20
	mediaUnavailable    = "Some attachments could not be imported into Multica. Check that the bot can access them and that each file is no larger than 20 MiB (up to 10 files per message)."
)

type mediaStorage interface {
	ObjectURL(key string) string
	Upload(ctx context.Context, key string, data []byte, contentType, filename string) (string, error)
}

type mediaResolver struct {
	decrypt Decrypter
	storage mediaStorage
	ledger  engine.MediaIntentLedger
	client  *http.Client
	logger  *slog.Logger
}

var (
	_ engine.MediaResolver        = (*mediaResolver)(nil)
	_ engine.MediaAbandonNotifier = (*mediaResolver)(nil)
)

func NewMediaResolver(decrypt Decrypter, store mediaStorage, ledger engine.MediaIntentLedger, client *http.Client, logger *slog.Logger) engine.MediaResolver {
	if logger == nil {
		logger = slog.Default()
	}
	return &mediaResolver{decrypt: decrypt, storage: store, ledger: ledger, client: client, logger: logger}
}

func (r *mediaResolver) HasMedia(msg channel.InboundMessage) bool {
	raw, err := decodeMattermostRaw(msg)
	return err == nil && len(raw.FileIDs) > 0
}

func (r *mediaResolver) ResolveMedia(ctx context.Context, inst engine.ResolvedInstallation, _ engine.ResolvedIdentity, _ pgtype.UUID, chatMessageID pgtype.UUID, msg channel.InboundMessage) channel.InboundMessage {
	raw, err := decodeMattermostRaw(msg)
	if err != nil || len(raw.FileIDs) == 0 {
		return msg
	}
	client, err := r.clientForInstallation(inst)
	if err != nil {
		r.logger.WarnContext(ctx, "mattermost: prepare attachment import failed", "message_id", msg.MessageID, "error", err)
		return msg
	}
	files := mergeFileIDs(raw.FileIDs)
	failed := len(files) > maxInboundFiles
	if failed {
		files = files[:maxInboundFiles]
	}
	for i, fileID := range files {
		if ctx.Err() != nil {
			break
		}
		budget := defaultTimeout
		if deadline, ok := ctx.Deadline(); ok {
			budget = min(budget, time.Until(deadline)/time.Duration(len(files)-i))
		}
		fileCtx, cancel := context.WithTimeout(ctx, budget)
		ref, err := r.importFile(fileCtx, client, inst, chatMessageID, fileID, i)
		cancel()
		if err != nil {
			r.logger.WarnContext(ctx, "mattermost: attachment import failed", "message_id", msg.MessageID, "file_index", i, "error", err)
			failed = true
			continue
		}
		msg.MediaRefs = append(msg.MediaRefs, ref)
	}
	if failed && ctx.Err() == nil {
		budget := 2 * time.Second
		if deadline, ok := ctx.Deadline(); ok {
			budget = min(budget, time.Until(deadline)/2)
		}
		noticeCtx, cancel := context.WithTimeout(ctx, budget)
		r.notifyUnavailable(noticeCtx, client, msg)
		cancel()
	}
	return msg
}

func (r *mediaResolver) clientForInstallation(inst engine.ResolvedInstallation) (*restClient, error) {
	row, ok := inst.Platform.(db.ChannelInstallation)
	if !ok {
		return nil, errors.New("installation platform row unavailable")
	}
	creds, err := decodeCredentials(row.Config, r.decrypt)
	if err != nil {
		return nil, err
	}
	return newRESTClient(creds.ServerURL, creds.AccessToken, r.client), nil
}

func (r *mediaResolver) importFile(ctx context.Context, client *restClient, inst engine.ResolvedInstallation, chatMessageID pgtype.UUID, fileID string, index int) (channel.MediaRef, error) {
	filePath := "/files/" + url.PathEscape(fileID)
	var info struct {
		Name     string `json:"name"`
		MimeType string `json:"mime_type"`
		Size     int64  `json:"size"`
	}
	if err := client.do(ctx, http.MethodGet, filePath+"/info", nil, &info); err != nil {
		return channel.MediaRef{}, fmt.Errorf("read file metadata: %w", err)
	}
	if info.Size > maxInboundFileBytes {
		return channel.MediaRef{}, errors.New("file exceeds the 20 MiB limit")
	}
	resp, err := client.request(ctx, http.MethodGet, filePath, nil)
	if err != nil {
		return channel.MediaRef{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return channel.MediaRef{}, decodeAPIError(resp)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxInboundFileBytes+1))
	if err != nil {
		return channel.MediaRef{}, fmt.Errorf("read file bytes: %w", err)
	}
	if len(data) > maxInboundFileBytes {
		return channel.MediaRef{}, errors.New("file exceeds the 20 MiB limit")
	}
	contentType, _, _ := mime.ParseMediaType(info.MimeType)
	if contentType == "" || contentType == "application/octet-stream" {
		contentType, _, _ = mime.ParseMediaType(http.DetectContentType(data))
	}
	filename := path.Base(strings.ReplaceAll(info.Name, "\\", "/"))
	if filename == "." || filename == ".." || filename == "/" {
		filename = "attachment"
	}
	key := path.Join("workspaces", util.UUIDToString(inst.WorkspaceID), "mattermost", util.UUIDToString(inst.ID), util.UUIDToString(chatMessageID), strconv.Itoa(index))
	link := r.storage.ObjectURL(key)
	owned, err := r.ledger.RecordPendingMediaObject(ctx, engine.RecordPendingMediaObjectParams{
		StorageKey: key, WorkspaceID: inst.WorkspaceID, ChatMessageID: chatMessageID, StorageURL: link, InstallationID: inst.ID,
	})
	if err != nil {
		return channel.MediaRef{}, fmt.Errorf("record media intent: %w", err)
	}
	if !owned {
		return channel.MediaRef{}, errors.New("attachment intent owned by reconciler")
	}
	if _, err := r.storage.Upload(ctx, key, data, contentType, filename); err != nil {
		return channel.MediaRef{}, fmt.Errorf("store attachment: %w", err)
	}
	kind := channel.MsgTypeFile
	if strings.HasPrefix(contentType, "image/") {
		kind = channel.MsgTypeImage
	}
	return channel.MediaRef{Type: kind, StorageKey: key, StorageURL: link, Filename: filename, MimeType: contentType, SizeBytes: int64(len(data))}, nil
}

func (r *mediaResolver) NotifyMediaAbandoned(ctx context.Context, inst engine.ResolvedInstallation, msg channel.InboundMessage, _ engine.MediaAbandonment) {
	client, err := r.clientForInstallation(inst)
	if err != nil {
		r.logger.WarnContext(ctx, "mattermost: prepare attachment notice failed", "message_id", msg.MessageID, "error", err)
		return
	}
	r.notifyUnavailable(ctx, client, msg)
}

func (r *mediaResolver) notifyUnavailable(ctx context.Context, client *restClient, msg channel.InboundMessage) {
	_, _, thread := mattermostSessionRouting(msg)
	if _, err := client.CreatePost(ctx, Post{ChannelID: msg.Source.ChatID, RootID: thread, Message: mediaUnavailable}); err != nil {
		r.logger.WarnContext(ctx, "mattermost: attachment notice failed", "message_id", msg.MessageID, "error", err)
	}
}
