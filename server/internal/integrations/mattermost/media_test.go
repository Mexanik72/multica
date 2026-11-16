package mattermost

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/multica-ai/multica/server/internal/integrations/channel"
	"github.com/multica-ai/multica/server/internal/integrations/channel/engine"
	"github.com/multica-ai/multica/server/internal/storage"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

type recordingMediaLedger struct {
	intents []engine.RecordPendingMediaObjectParams
}

func (l *recordingMediaLedger) RecordPendingMediaObject(_ context.Context, p engine.RecordPendingMediaObjectParams) (bool, error) {
	l.intents = append(l.intents, p)
	return true, nil
}

func TestMediaImportStoresReadableZipWhenAnotherFileFails(t *testing.T) {
	var archive bytes.Buffer
	zipWriter := zip.NewWriter(&archive)
	logFile, err := zipWriter.Create("checkout.log")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.WriteString(logFile, "checkout returned 503"); err != nil {
		t.Fatal(err)
	}
	if err := zipWriter.Close(); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer fixture-token" {
			t.Error("attachment request did not authenticate as the bot")
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		switch r.Method + " " + r.URL.Path {
		case "GET /api/v4/files/inaccessible/info":
			w.WriteHeader(http.StatusForbidden)
		case "GET /api/v4/files/zip/info":
			_, _ = io.WriteString(w, `{"name":"logs.zip","mime_type":"application/zip","size":100}`)
		case "GET /api/v4/files/zip":
			_, _ = w.Write(archive.Bytes())
		case "POST /api/v4/posts":
			var notice Post
			if err := json.NewDecoder(r.Body).Decode(&notice); err != nil || notice.ChannelID != "channel" || notice.RootID != "root" {
				t.Errorf("attachment failure notice = %+v, decode error = %v", notice, err)
			}
			_, _ = io.WriteString(w, `{"id":"notice"}`)
		default:
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)

	t.Setenv("LOCAL_UPLOAD_DIR", t.TempDir())
	t.Setenv("LOCAL_UPLOAD_BASE_URL", "https://multica.example")
	store := storage.NewLocalStorageFromEnv()
	ledger := &recordingMediaLedger{}
	resolver := NewMediaResolver(nil, store, ledger, nil, nil)
	config, err := json.Marshal(installConfig{ServerURL: server.URL, AccessTokenEncrypted: base64.StdEncoding.EncodeToString([]byte("fixture-token"))})
	if err != nil {
		t.Fatal(err)
	}
	messageID := util.MustParseUUID("10000000-0000-0000-0000-000000000001")
	msg := channel.InboundMessage{MessageID: "mention", Raw: json.RawMessage(`{"file_ids":["inaccessible","zip","zip"]}`),
		Source: channel.Source{ChatID: "channel", ChatType: channel.ChatTypeGroup, ThreadID: "root"}}
	inst := engine.ResolvedInstallation{ID: messageID, WorkspaceID: messageID, Platform: db.ChannelInstallation{Config: config}}
	resolved := resolver.ResolveMedia(t.Context(), inst, engine.ResolvedIdentity{}, pgtype.UUID{}, messageID, msg)
	if len(resolved.MediaRefs) != 1 {
		t.Fatalf("attachment references = %+v, want one imported ZIP", resolved.MediaRefs)
	}
	ref := resolved.MediaRefs[0]
	if ref.Filename != "logs.zip" || ref.MimeType != "application/zip" || ref.SizeBytes != int64(archive.Len()) {
		t.Errorf("unexpected ZIP metadata: %+v", ref)
	}
	if len(ledger.intents) != 1 || ledger.intents[0].StorageKey != ref.StorageKey || ledger.intents[0].ChatMessageID != messageID {
		t.Errorf("unexpected storage intents: %+v", ledger.intents)
	}
	reader, err := store.GetReader(t.Context(), ref.StorageKey)
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	data, err := io.ReadAll(reader)
	if err != nil || !bytes.Equal(data, archive.Bytes()) {
		t.Fatalf("stored ZIP bytes differ, read error = %v", err)
	}
}
