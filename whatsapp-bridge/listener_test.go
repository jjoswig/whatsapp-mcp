package main

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestBridgePortConfiguration(t *testing.T) {
	for _, tc := range []struct {
		value string
		want  int
	}{
		{"8081", 8081}, {" 9000 ", 9000}, {"1", 1}, {"65535", 65535},
	} {
		t.Run(tc.value, func(t *testing.T) {
			t.Setenv("WHATSAPP_BRIDGE_PORT", tc.value)
			port, err := bridgePortFromEnv()
			if err != nil || port != tc.want {
				t.Fatalf("got port %d, %v; expected %d", port, err, tc.want)
			}
		})
	}
	for _, value := range []string{"", "0", "-1", "65536", "invalid"} {
		t.Run("invalid_"+value, func(t *testing.T) {
			t.Setenv("WHATSAPP_BRIDGE_PORT", value)
			if _, err := bridgePortFromEnv(); err == nil {
				t.Fatal("invalid port must prevent startup")
			}
		})
	}
	t.Run("default", func(t *testing.T) {
		t.Setenv("WHATSAPP_BRIDGE_PORT", "temporary")
		os.Unsetenv("WHATSAPP_BRIDGE_PORT")
		if port, err := bridgePortFromEnv(); err != nil || port != 8080 {
			t.Fatalf("default port changed: %d, %v", port, err)
		}
	})
}

func TestPortConflictStopsBeforeAccountConnection(t *testing.T) {
	if os.Getenv("WHATSAPP_TEST_PORT_CONFLICT") == "1" {
		main()
		return
	}
	listener, err := openBridgeListener(0)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	port := listener.Addr().(*net.TCPAddr).Port
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestPortConflictStopsBeforeAccountConnection$")
	cmd.Env = append(os.Environ(), "WHATSAPP_TEST_PORT_CONFLICT=1", "WHATSAPP_READ_ONLY=true", "WHATSAPP_BRIDGE_PORT="+strconv.Itoa(port))
	cmd.Dir = t.TempDir()
	output, err := cmd.CombinedOutput()
	if ctx.Err() != nil || err == nil || !strings.Contains(string(output), "set WHATSAPP_BRIDGE_PORT") {
		t.Fatalf("expected immediate port conflict failure, got %v: %s", err, output)
	}
	if strings.Contains(string(output), "Checking WhatsApp Web") {
		t.Fatal("port conflict must be checked before contacting WhatsApp")
	}
	if _, err := os.Stat(filepath.Join(cmd.Dir, "store")); !os.IsNotExist(err) {
		t.Fatalf("port conflict must be checked before opening session storage: %v", err)
	}
}

func TestReservedListenerServesReadOnlyAPI(t *testing.T) {
	listener, err := openBridgeListener(0)
	if err != nil {
		t.Fatal(err)
	}
	if !listener.Addr().(*net.TCPAddr).IP.IsLoopback() {
		t.Fatal("bridge must bind to loopback")
	}
	server, result := (&bridge{readOnly: true}).startRESTServer(listener)
	defer server.Close()
	client := &http.Client{Timeout: time.Second}
	response, err := client.Post("http://"+listener.Addr().String()+"/api/send", "application/json", strings.NewReader(`{"recipient":"123","message":"blocked"}`))
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(response.Body)
	response.Body.Close()
	if err != nil || response.StatusCode != http.StatusForbidden || !strings.Contains(string(body), readOnlyMessage) {
		t.Fatalf("read-only listener did not reject send: %s, %v", body, err)
	}
	if err := server.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-result:
		if !errors.Is(err, http.ErrServerClosed) {
			t.Fatalf("unexpected shutdown result: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("server shutdown was not reported")
	}
}

func TestListenerFailureIsReported(t *testing.T) {
	listener, err := openBridgeListener(0)
	if err != nil {
		t.Fatal(err)
	}
	listener.Close()
	server, result := (&bridge{readOnly: true}).startRESTServer(listener)
	defer server.Close()
	select {
	case err := <-result:
		if err == nil {
			t.Fatal("serve failure was silently ignored")
		}
	case <-time.After(time.Second):
		t.Fatal("serve failure was not reported")
	}
}
