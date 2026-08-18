package cmd

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestContextCreateSupportsAllTypes(t *testing.T) {
	for _, contextType := range []string{"workspace", "memory", "knowledge", "artifacts"} {
		t.Run(contextType, func(t *testing.T) {
			isolateHome(t)
			var requestType string
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				switch r.URL.Path {
				case "/api/v1/namespaces":
					_, _ = w.Write([]byte(`{"namespaces":["team1"]}`))
				case "/api/v1/contexts":
					var body struct {
						Type string `json:"type"`
					}
					if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
						t.Fatal(err)
					}
					requestType = body.Type
					_, _ = w.Write([]byte(`{"name":"research","namespace":"team1","type":"` + body.Type + `","status":"provisioning","storage":{"backend":"pvc","size":"1Gi","accessMode":"ReadWriteOnce"},"attachment":{"kind":"pvc","claimName":"context-research"}}`))
				default:
					t.Fatalf("unexpected request: %s %s", r.Method, r.URL.Path)
				}
			}))
			defer srv.Close()
			setupImportContext(t, srv, "team1")

			if _, err := execute(t, "context", "create", "research", "--type", contextType); err != nil {
				t.Fatal(err)
			}
			if requestType != contextType {
				t.Fatalf("type = %q, want %q", requestType, contextType)
			}
		})
	}
}

func TestContextsList(t *testing.T) {
	isolateHome(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/v1/namespaces":
			_, _ = w.Write([]byte(`{"namespaces":["team1"]}`))
		case "/api/v1/contexts/team1":
			_, _ = w.Write([]byte(`{"items":[{"name":"research","namespace":"team1","type":"workspace","status":"ready","storage":{"backend":"pvc","size":"10Gi","accessMode":"ReadWriteMany"},"attachment":{"kind":"pvc","claimName":"context-research"}}]}`))
		default:
			t.Fatalf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
	}))
	defer srv.Close()
	setupImportContext(t, srv, "team1")

	out, err := execute(t, "contexts", "list")
	if err != nil {
		t.Fatal(err)
	}
	for _, expected := range []string{"research", "ReadWriteMany", "context-research"} {
		if !strings.Contains(out, expected) {
			t.Errorf("output missing %q:\n%s", expected, out)
		}
	}

	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) != 2 {
		t.Fatalf("expected header and one row, got:\n%s", out)
	}
	for _, columns := range [][2]string{
		{"TYPE", "workspace"},
		{"STATUS", "ready"},
		{"SIZE", "10Gi"},
		{"ACCESS MODE", "ReadWriteMany"},
		{"CLAIM", "context-research"},
	} {
		if strings.Index(lines[0], columns[0]) != strings.Index(lines[1], columns[1]) {
			t.Errorf("column %q is not aligned with %q:\n%s", columns[0], columns[1], out)
		}
	}
}

func TestContextAlias(t *testing.T) {
	command, _, err := rootCmd.Find([]string{"context", "list"})
	if err != nil {
		t.Fatal(err)
	}
	if command.Name() != "list" {
		t.Fatalf("resolved command = %q, want list", command.Name())
	}
}

func TestContextCommandsShowHelpWithoutName(t *testing.T) {
	for _, subcommand := range []string{"create", "get", "delete"} {
		out, err := execute(t, "context", subcommand)
		if err != nil {
			t.Fatalf("context %s: %v", subcommand, err)
		}
		if !strings.Contains(out, "Usage:") || !strings.Contains(out, subcommand+" NAME") {
			t.Errorf("context %s did not show command help:\n%s", subcommand, out)
		}
	}
}

func TestContextCreateHelpIncludesExamples(t *testing.T) {
	out, err := execute(t, "context", "create", "--help")
	if err != nil {
		t.Fatal(err)
	}
	for _, expected := range []string{
		"Types classify how the stored data is intended to be used:",
		"workspace   Mutable files used while an agent works",
		"memory      Durable observations and experiences",
		"knowledge   Synthesized, reusable understanding",
		"artifacts   Produced reports, media, and other outputs",
		"same PVC-backed storage and lifecycle behavior",
		"Examples:",
		"context create research",
		"--type memory",
		"--shared",
		"--context research:/workspace",
		"workspace, memory, knowledge, or artifacts",
	} {
		if !strings.Contains(out, expected) {
			t.Errorf("create help missing %q:\n%s", expected, out)
		}
	}
}
