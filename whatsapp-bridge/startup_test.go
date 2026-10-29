package main

import (
	"context"
	"errors"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/store"
	"go.mau.fi/whatsmeow/store/sqlstore"
	"go.mau.fi/whatsmeow/types/events"
	waLog "go.mau.fi/whatsmeow/util/log"
)

type startupTransport func(*http.Request) (*http.Response, error)

func (f startupTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestRefreshWhatsAppVersion(t *testing.T) {
	original := store.GetWAVersion()
	t.Cleanup(func() { store.SetWAVersion(original) })
	client := &http.Client{Transport: startupTransport(func(r *http.Request) (*http.Response, error) {
		if r.Method != http.MethodGet || r.URL.Host != "web.whatsapp.com" {
			t.Fatalf("version refresh must only read the public WhatsApp page: %s %s", r.Method, r.URL)
		}
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{"client_revision":1234567890,}`))}, nil
	})}
	if err := refreshWhatsAppVersion(client); err != nil {
		t.Fatal(err)
	}
	if got := store.GetWAVersion(); got != (store.WAVersionContainer{2, 3000, 1234567890}) {
		t.Fatalf("version was not refreshed: %s", got)
	}
}

func TestVersionRefreshFailureKeepsBundledVersion(t *testing.T) {
	original := store.GetWAVersion()
	for _, tc := range []struct {
		name, body string
		status     int
		err        error
	}{
		{name: "offline", err: errors.New("network unavailable")},
		{name: "server failure", status: 503, body: "unavailable"},
		{name: "unrecognized page", status: 200, body: "no version here"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client := &http.Client{Transport: startupTransport(func(*http.Request) (*http.Response, error) {
				if tc.err != nil {
					return nil, tc.err
				}
				return &http.Response{StatusCode: tc.status, Body: io.NopCloser(strings.NewReader(tc.body))}, nil
			})}
			if err := refreshWhatsAppVersion(client); err == nil {
				t.Fatal("expected a version refresh error")
			}
			if got := store.GetWAVersion(); got != original {
				t.Fatalf("failed refresh changed the version: %s", got)
			}
		})
	}
}

func TestLoginDeadlineIncludesWaitingForFirstQRCode(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	qr := make(chan whatsmeow.QRChannelItem)
	err := waitForWhatsAppLogin(ctx, qr, make(chan error), func(string) { t.Fatal("unexpected QR") })
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("waiting without a QR must time out: %v", err)
	}
}

func TestLoginRequiresAuthenticationAfterPairing(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	qr := make(chan whatsmeow.QRChannelItem, 1)
	qr <- whatsmeow.QRChannelSuccess
	close(qr)
	err := waitForWhatsAppLogin(ctx, qr, make(chan error), func(string) { t.Fatal("unexpected QR") })
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("pairing alone must not report a logged-in connection: %v", err)
	}
}

func TestLoginDisplaysQRAndWaitsForAuthenticatedEvent(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	qr := make(chan whatsmeow.QRChannelItem)
	result := make(chan error)
	go func() {
		defer close(qr)
		for _, item := range []whatsmeow.QRChannelItem{{Event: "code", Code: "test-qr"}, whatsmeow.QRChannelSuccess} {
			select {
			case qr <- item:
			case <-ctx.Done():
				return
			}
		}
		select {
		case result <- nil:
		case <-ctx.Done():
		}
	}()
	var shown string
	err := waitForWhatsAppLogin(ctx, qr, result, func(code string) { shown = code })
	if err != nil || shown != "test-qr" {
		t.Fatalf("expected displayed QR and successful login, got %q, %v", shown, err)
	}
}

func TestLoginHandlesTerminalQREvents(t *testing.T) {
	for _, item := range []whatsmeow.QRChannelItem{
		whatsmeow.QRChannelClientOutdated, whatsmeow.QRChannelTimeout,
		whatsmeow.QRChannelErrUnexpectedEvent, whatsmeow.QRChannelScannedWithoutMultidevice,
		{Event: "error", Error: errors.New("pairing rejected")},
	} {
		t.Run(item.Event, func(t *testing.T) {
			qr := make(chan whatsmeow.QRChannelItem, 1)
			qr <- item
			close(qr)
			err := waitForWhatsAppLogin(context.Background(), qr, nil, func(string) { t.Fatal("unexpected QR") })
			if err == nil {
				t.Fatal("terminal QR event must fail instead of hanging")
			}
			if item.Error != nil && !errors.Is(err, item.Error) {
				t.Fatalf("pairing error was lost: %v", err)
			}
		})
	}
	qr := make(chan whatsmeow.QRChannelItem)
	close(qr)
	if err := waitForWhatsAppLogin(context.Background(), qr, nil, nil); err == nil {
		t.Fatal("closed QR channel must not be treated as success")
	}
}

func TestSavedSessionReportsAuthenticationResult(t *testing.T) {
	for _, tc := range []struct {
		name string
		evt  interface{}
		want string
	}{
		{"connected", &events.Connected{}, ""},
		{"outdated", &events.ClientOutdated{}, "405"},
		{"rejected", &events.ConnectFailure{Message: "rejected"}, "rejected"},
		{"logged out", &events.LoggedOut{}, "logged out"},
		{"pairing error", &events.PairError{Error: errors.New("bad signature")}, "bad signature"},
		{"restricted", &events.TemporaryBan{}, "restricted"},
		{"stream error", &events.StreamError{Code: "500"}, "500"},
		{"duplicate session", &events.StreamReplaced{}, "duplicate bridge"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			result := make(chan error, 1)
			reportConnectionEvent(tc.evt, result)
			err := waitForWhatsAppLogin(context.Background(), nil, result, nil)
			if tc.want == "" {
				if err != nil {
					t.Fatal(err)
				}
			} else if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("expected error containing %q, got %v", tc.want, err)
			}
			// Subsequent events must not block the main WhatsApp event handler.
			reportConnectionEvent(tc.evt, result)
			reportConnectionEvent(tc.evt, result)
		})
	}
}

func TestUnrelatedEventsDoNotReportLoginSuccess(t *testing.T) {
	result := make(chan error, 1)
	reportConnectionEvent(&events.Disconnected{}, result)
	reportConnectionEvent(&events.Message{}, result)
	if len(result) != 0 {
		t.Fatal("unrelated events must not complete startup")
	}
	close(result)
	if err := waitForWhatsAppLogin(context.Background(), nil, result, nil); err == nil {
		t.Fatal("closed result channel must not be treated as successful login")
	}
}

func TestSessionStoreInitializesWithUpdatedLibrary(t *testing.T) {
	ctx := context.Background()
	address := "file:" + filepath.Join(t.TempDir(), "session.db") + "?_foreign_keys=on"
	for attempt := 0; attempt < 2; attempt++ {
		container, err := sqlstore.New(ctx, "sqlite3", address, waLog.Noop)
		if err != nil {
			t.Fatal(err)
		}
		device, err := container.GetFirstDevice(ctx)
		closeErr := container.Close()
		if err != nil || device == nil || device.ID != nil {
			t.Fatalf("fresh store must provide a device ready for QR pairing: %v, %v", device, err)
		}
		if closeErr != nil {
			t.Fatal(closeErr)
		}
	}
}
