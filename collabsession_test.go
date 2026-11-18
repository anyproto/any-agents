package main

import (
	"encoding/json"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"
)

func inSession(t *testing.T, sid string) {
	t.Helper()
	t.Setenv("ANY_AGENTS_TOOL", "claude")
	t.Setenv("CLAUDE_CODE_SESSION_ID", sid)
}

// A session reads at its own pace; one without a cursor starts from the
// collab's plain cursor.
func TestCursorPerSession(t *testing.T) {
	t.Setenv("ANY_AGENTS_HOME", t.TempDir())
	if _, ok := loadCursor("obj", "s1"); ok {
		t.Fatal("a cursor before anything was saved")
	}
	saveCursor("obj", "s1", "v5")
	if _, ok := loadCursor("obj", "s2"); ok {
		t.Fatal("s2 sees s1's cursor")
	}
	saveCursor("obj", "", "v3")
	if v, ok := loadCursor("obj", "s2"); !ok || v != "v3" {
		t.Fatalf("s2 without a cursor = %q, %v; want the plain cursor v3", v, ok)
	}
	if v, _ := loadCursor("obj", "s1"); v != "v5" {
		t.Fatalf("s1 cursor = %q, want v5", v)
	}
	saveCursor("obj", "s1", "")
	if v, ok := loadCursor("obj", "s1"); !ok || v != "" {
		t.Fatalf("an empty cursor = %q, %v; want \"\", true", v, ok)
	}
}

func TestPostedBySession(t *testing.T) {
	t.Setenv("ANY_AGENTS_HOME", t.TempDir())
	recordPosted("obj", "s1", "m1")
	recordPosted("obj", "s2", "m2")
	recordPosted("obj", "", "m3")
	recordPosted("obj", "s1", "m4")
	for sid, want := range map[string]string{"s1": "m1,m4", "s2": "m2", "": "m3", "s3": ""} {
		var got []string
		for _, m := range []string{"m1", "m2", "m3", "m4"} {
			if postedBy("obj", sid)[m] {
				got = append(got, m)
			}
		}
		if strings.Join(got, ",") != want {
			t.Errorf("postedBy(%q) = %v, want %s", sid, got, want)
		}
	}
}

// chatServer answers every chat read with msgs and every object read
// with an open collab.
func chatServer(t *testing.T, msgs []map[string]any) *Client {
	return newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/query") {
			_ = json.NewEncoder(w).Encode(map[string]any{"records": msgs})
			return
		}
		_, _ = w.Write([]byte(`{}`))
	})
}

func chatMsg(id, ver, text string) map[string]any {
	return map[string]any{"id": id, "text": text, "creator": "me", "agent": map[string]any{"name": "Claude (me)", "done": true},
		"_ver": map[string]any{"id": ver}}
}

// Two sessions of one member in one collab: each one's wait wakes on the
// other's message and skips its own.
func TestWaitWakesOnSiblingSession(t *testing.T) {
	c := chatServer(t, []map[string]any{chatMsg("m1", "v1", "request from s1"), chatMsg("m2", "v2", "answer from s2")})
	recordPosted("obj", "s1", "m1")
	recordPosted("obj", "s2", "m2")
	for sid, want := range map[string]string{"s1": "answer from s2", "s2": "request from s1"} {
		inSession(t, sid)
		saveCursor("obj", sid, "")
		out, closed, err := collabWait(c, &State{Identity: "me"}, &Space{Id: "sp"}, "obj", 5*time.Second, false)
		if err != nil || closed != "" || len(out) != 1 || out[0]["text"] != want {
			t.Errorf("%s: wait = %v, %q, %v; want only %q", sid, out, closed, err, want)
		}
	}
}

// A session's own message does not wake its wait, and the wait that
// skipped it leaves the sibling's cursor where it was.
func TestWaitSkipsOwnMessage(t *testing.T) {
	c := chatServer(t, []map[string]any{chatMsg("m1", "v1", "my own")})
	recordPosted("obj", "s1", "m1")
	inSession(t, "s1")
	saveCursor("obj", "s1", "")
	saveCursor("obj", "s2", "")
	out, _, err := collabWait(c, &State{Identity: "me"}, &Space{Id: "sp"}, "obj", 2*time.Second, false)
	if err != nil || len(out) != 0 {
		t.Fatalf("wait on an own message = %v, %v; want a timeout", out, err)
	}
	if v, _ := loadCursor("obj", "s2"); v != "" {
		t.Fatalf("s1's wait moved s2's cursor to %q", v)
	}
	if v, _ := loadCursor("obj", "s1"); v != "v1" {
		t.Fatalf("s1's cursor = %q, want v1", v)
	}
	if n := newMessageCount(c, &Space{Id: "sp"}, "obj", "s2", 10); n != 1 {
		t.Fatalf("s2 counts %d new messages, want 1", n)
	}
	if n := newMessageCount(c, &Space{Id: "sp"}, "obj", "s1", 10); n != 0 {
		t.Fatalf("s1 counts %d new messages, want 0 (its own)", n)
	}
}

// A line names its signer by id, whatever name the member or the agent
// label claims.
func TestPrintMessagesTagsSigner(t *testing.T) {
	r, w, _ := os.Pipe()
	stdout := os.Stdout
	os.Stdout = w
	printMessages([]map[string]any{
		{"creator": "A9FFFFFFFr7yXRqr", "text": "steer", "_ver": map[string]any{"id": "v1"}},
		{"creator": "B7xxxxxxxxxxxxxx", "text": "spoof", "agent": map[string]any{"name": "cheggaaa"}, "_ver": map[string]any{"id": "v2"}},
		{"creator": "C5unknownmember1", "text": "who", "_ver": map[string]any{"id": "v3"}},
	}, map[string]string{"A9FFFFFFFr7yXRqr": "cheggaaa", "B7xxxxxxxxxxxxxx": "roma"})
	w.Close()
	os.Stdout = stdout
	b, _ := io.ReadAll(r)
	out := string(b)
	for _, want := range []string{"cheggaaa [A9FFFFFF]: steer", "cheggaaa [B7xxxxxx]: spoof", "] [C5unknow]: who"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
}
