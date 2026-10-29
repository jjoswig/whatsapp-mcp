package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"go.mau.fi/whatsmeow"
)

func TestReadOnlyConfiguration(t *testing.T) {
	for _, value := range []string{"true", "1", "yes", "on", " TRUE ", "Yes"} {
		t.Run(value, func(t *testing.T) {
			t.Setenv("WHATSAPP_READ_ONLY", value)
			got, err := readOnlyFromEnv()
			if err != nil || !got {
				t.Fatalf("got %t, %v; expected read-only", got, err)
			}
		})
	}
	for _, value := range []string{"false", "0", "no", "off", " FALSE ", "No"} {
		t.Run(value, func(t *testing.T) {
			t.Setenv("WHATSAPP_READ_ONLY", value)
			got, err := readOnlyFromEnv()
			if err != nil || got {
				t.Fatalf("got %t, %v; expected writes enabled", got, err)
			}
		})
	}
	for _, value := range []string{"", "tru", "enabled", "2"} {
		t.Run("invalid_"+value, func(t *testing.T) {
			t.Setenv("WHATSAPP_READ_ONLY", value)
			if _, err := readOnlyFromEnv(); err == nil {
				t.Fatal("invalid value must prevent startup")
			}
		})
	}
	t.Run("unset", func(t *testing.T) {
		t.Setenv("WHATSAPP_READ_ONLY", "temporary")
		os.Unsetenv("WHATSAPP_READ_ONLY")
		if got, err := readOnlyFromEnv(); err != nil || got {
			t.Fatalf("unset must preserve writable default: %t, %v", got, err)
		}
	})
}

func TestInvalidConfigurationStopsBeforeConnecting(t *testing.T) {
	if os.Getenv("WHATSAPP_TEST_INVALID_CONFIG") == "1" {
		main()
		return
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestInvalidConfigurationStopsBeforeConnecting$")
	cmd.Env = append(os.Environ(), "WHATSAPP_TEST_INVALID_CONFIG=1", "WHATSAPP_READ_ONLY=typo")
	cmd.Dir = t.TempDir()
	output, err := cmd.CombinedOutput()
	if err == nil || !strings.Contains(string(output), "invalid WHATSAPP_READ_ONLY") {
		t.Fatalf("expected configuration failure, got %v: %s", err, output)
	}
	if _, err := os.Stat(filepath.Join(cmd.Dir, "store")); !os.IsNotExist(err) {
		t.Fatalf("configuration must be validated before creating session storage: %v", err)
	}
}

func TestReadOnlyDeniesUnreviewedRoutesBeforeHandlers(t *testing.T) {
	b := &bridge{readOnly: true}
	handler := b.restrictWrites(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatalf("denied request reached handler: %s %s", r.Method, r.URL.Path)
	}))
	paths := []string{
		"/api/send", "/api/reply", "/api/archive", "/api/groups/remove",
		"/api/delete", "/api/future-action", "/api/send/", "/api/%73end",
		"//api/download", "/api/download/", "/api/x/../download",
	}
	for _, path := range paths {
		for _, method := range []string{"GET", "HEAD", "POST", "PUT", "PATCH", "DELETE", "OPTIONS"} {
			t.Run(method+path, func(t *testing.T) {
				response := httptest.NewRecorder()
				handler.ServeHTTP(response, httptest.NewRequest(method, path, strings.NewReader("invalid JSON")))
				if response.Code != http.StatusForbidden {
					t.Fatalf("got HTTP %d; expected 403", response.Code)
				}
				var result SendMessageResponse
				if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil || result.Success || result.Message != readOnlyMessage {
					t.Fatalf("unexpected denial response: %s (%v)", response.Body, err)
				}
			})
		}
	}
	for _, method := range []string{"GET", "HEAD", "PUT", "PATCH", "DELETE", "OPTIONS"} {
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, httptest.NewRequest(method, "/api/download", nil))
		if response.Code != http.StatusForbidden {
			t.Fatalf("download allowlist must include the method: %s got %d", method, response.Code)
		}
	}
}

func TestReadOnlySendRejectsAllPayloads(t *testing.T) {
	// A nil client and store ensure no WhatsApp operations can run in this test.
	b := &bridge{readOnly: true}
	for _, payload := range []string{
		`{"recipient":"123","message":"hello"}`,
		`{"recipient":"123@g.us","message":"reply","quoted_message_id":"id"}`,
		`{"recipient":"123","media_path":"/private/photo.jpg"}`,
		`{"recipient":"123","media_path":"/private/voice.ogg"}`,
		`{"recipient":"123","media_path":"/private/document.pdf","read_only":false}`,
		`not JSON`,
	} {
		response := httptest.NewRecorder()
		b.restHandler().ServeHTTP(response, httptest.NewRequest("POST", "/api/send", strings.NewReader(payload)))
		if response.Code != http.StatusForbidden {
			t.Fatalf("payload %s got HTTP %d", payload, response.Code)
		}
	}
	for _, media := range []string{"", "/private/photo.jpg", "/private/voice.ogg", "/private/document.pdf"} {
		if ok, message := b.sendWhatsAppMessage("123", "hello", media); ok || message != readOnlyMessage {
			t.Fatalf("direct helper bypassed policy: %t, %s", ok, message)
		}
	}
}

func TestReadOnlyBlocksHistorySyncSend(t *testing.T) {
	// The original helper sends a protocol message outside the REST send route.
	output, err := os.CreateTemp(t.TempDir(), "history-output")
	if err != nil {
		t.Fatal(err)
	}
	defer output.Close()
	stdout := os.Stdout
	os.Stdout = output
	t.Cleanup(func() { os.Stdout = stdout })
	(&bridge{readOnly: true}).requestHistorySync()
	os.Stdout = stdout
	if _, err := output.Seek(0, io.SeekStart); err != nil {
		t.Fatal(err)
	}
	data, err := io.ReadAll(output)
	if err != nil || !strings.Contains(string(data), readOnlyMessage) {
		t.Fatalf("history sync did not stop at write guard: %s (%v)", data, err)
	}
}

func TestReadOnlyDownloadRemainsAvailable(t *testing.T) {
	t.Chdir(t.TempDir())
	store, err := NewMessageStore()
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	chat := "123@s.whatsapp.net"
	if err := store.StoreChat(chat, "Test", time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := store.StoreMessage("media-id", chat, "123", "", time.Now(), false, "image", "photo.jpg", "", nil, nil, nil, 0); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join("store", chat)
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "photo.jpg"), []byte("cached media"), 0600); err != nil {
		t.Fatal(err)
	}
	b := &bridge{readOnly: true, messageStore: store}
	response := httptest.NewRecorder()
	b.restHandler().ServeHTTP(response, httptest.NewRequest("POST", "/api/download", strings.NewReader(`{"message_id":"media-id","chat_jid":"123@s.whatsapp.net"}`)))
	var result DownloadMediaResponse
	if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil || response.Code != http.StatusOK || !result.Success {
		t.Fatalf("read-only download failed: HTTP %d %s (%v)", response.Code, response.Body, err)
	}
	data, err := os.ReadFile(result.Path)
	if err != nil || string(data) != "cached media" {
		t.Fatalf("unexpected media: %s (%v)", data, err)
	}
}

func TestWritableModePreservesHandlers(t *testing.T) {
	b := &bridge{}
	for _, tc := range []struct {
		method, path, body string
		want               int
	}{
		{"GET", "/api/send", "", http.StatusMethodNotAllowed},
		{"POST", "/api/send", "invalid", http.StatusBadRequest},
		{"POST", "/api/send", `{"recipient":"123","message":"hello"}`, http.StatusInternalServerError},
		{"POST", "/api/future-action", "", http.StatusNotFound},
	} {
		response := httptest.NewRecorder()
		b.restHandler().ServeHTTP(response, httptest.NewRequest(tc.method, tc.path, strings.NewReader(tc.body)))
		if response.Code != tc.want {
			t.Fatalf("%s %s: got %d, expected %d", tc.method, tc.path, response.Code, tc.want)
		}
		if strings.Contains(response.Body.String(), readOnlyMessage) {
			t.Fatal("writable request was blocked by read-only policy")
		}
	}
	if ok, message := b.sendWhatsAppMessage("123", "hello", ""); ok || message != "Not connected to WhatsApp" {
		t.Fatalf("writable helper did not reach connection check: %t, %s", ok, message)
	}
}

func TestReadOnlyDisablesOutgoingMessageRetries(t *testing.T) {
	client := &whatsmeow.Client{}
	(&bridge{client: client, readOnly: true}).configureClient()
	if client.PreRetryCallback == nil || client.PreRetryCallback(nil, "message-id", 1, nil) {
		t.Fatal("read-only client must reject automatic outgoing message retries")
	}

	writableClient := &whatsmeow.Client{}
	(&bridge{client: writableClient}).configureClient()
	if writableClient.PreRetryCallback != nil {
		t.Fatal("writable mode must preserve default retry behavior")
	}
}
