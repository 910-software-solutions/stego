package compiler

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/coder/websocket"
)

// A small driver of a headless browser over the DevTools protocol, from Go (ST-STD-LOCAL; the owner's decision
// Q-G (1) keeps this driver and not Playwright). It starts the browser that STEGO_BROWSER names, with no network
// for the page: every request that is not a file: or data: address fails and is counted.

// cdpBrowser is one headless browser with one page attached.
type cdpBrowser struct {
	t       *testing.T
	cmd     *exec.Cmd
	conn    *websocket.Conn
	ctx     context.Context
	cancel  context.CancelFunc
	session string
	mu      sync.Mutex
	next    int
	waiting map[int]chan cdpReply
	blocked []string
	errors  []string
}

type cdpReply struct {
	Result json.RawMessage `json:"result"`
	Error  *struct {
		Message string `json:"message"`
	} `json:"error"`
}

type cdpMessage struct {
	ID        int             `json:"id"`
	Method    string          `json:"method"`
	SessionID string          `json:"sessionId"`
	Params    json.RawMessage `json:"params"`
	Result    json.RawMessage `json:"result"`
	Error     *struct {
		Message string `json:"message"`
	} `json:"error"`
}

// startBrowser starts the browser, or skips the test when no browser is named. A run in CI must name one.
func startBrowser(t *testing.T) *cdpBrowser {
	t.Helper()
	path := os.Getenv("STEGO_BROWSER")
	if path == "" {
		if os.Getenv("STEGO_REQUIRE_BROWSER_VIEW") == "1" {
			t.Fatal("STEGO_REQUIRE_BROWSER_VIEW is set, and STEGO_BROWSER names no browser")
		}
		t.Skip("set STEGO_BROWSER to a Chrome or Chromium executable to run the browser tests of the architecture view")
	}
	dir := t.TempDir()
	args := []string{"--headless=new", "--remote-debugging-port=0", "--user-data-dir=" + dir, "--no-first-run",
		"--no-default-browser-check", "--disable-background-networking", "--disable-gpu", "--disable-dev-shm-usage",
		"--disable-extensions", "--disable-sync", "--mute-audio", "--window-size=1280,900", "about:blank"}
	if os.Getenv("STEGO_BROWSER_NO_SANDBOX") == "1" {
		args = append([]string{"--no-sandbox"}, args...)
	}
	cmd := exec.Command(path, args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	log, err := os.Create(filepath.Join(dir, "browser.log"))
	if err != nil {
		t.Fatal(err)
	}
	cmd.Stdout, cmd.Stderr = log, log
	if err := cmd.Start(); err != nil {
		t.Fatalf("cannot start the browser: %v", err)
	}
	b := &cdpBrowser{t: t, cmd: cmd, waiting: map[int]chan cdpReply{}}
	t.Cleanup(b.stop)
	var ws string
	for deadline := time.Now().Add(20 * time.Second); time.Now().Before(deadline); time.Sleep(100 * time.Millisecond) {
		data, err := os.ReadFile(filepath.Join(dir, "DevToolsActivePort"))
		if lines := strings.Split(strings.TrimSpace(string(data)), "\n"); err == nil && len(lines) == 2 {
			ws = "ws://127.0.0.1:" + lines[0] + lines[1]
			break
		}
	}
	if ws == "" {
		t.Fatal("the browser wrote no DevTools port")
	}
	b.ctx, b.cancel = context.WithTimeout(context.Background(), 3*time.Minute)
	b.conn, _, err = websocket.Dial(b.ctx, ws, nil)
	if err != nil {
		t.Fatalf("cannot reach the DevTools socket: %v", err)
	}
	b.conn.SetReadLimit(64 << 20)
	go b.read()
	var target struct {
		TargetID string `json:"targetId"`
	}
	b.call("", "Target.createTarget", map[string]any{"url": "about:blank"}, &target)
	var attached struct {
		SessionID string `json:"sessionId"`
	}
	b.call("", "Target.attachToTarget", map[string]any{"targetId": target.TargetID, "flatten": true}, &attached)
	b.session = attached.SessionID
	b.do("Runtime.enable", nil)
	b.do("Page.enable", nil)
	b.do("Log.enable", nil)
	b.do("Fetch.enable", map[string]any{"patterns": []map[string]any{{"urlPattern": "*"}}})
	return b
}

func (b *cdpBrowser) stop() {
	if b.cancel != nil {
		b.cancel()
	}
	if b.conn != nil {
		_ = b.conn.CloseNow()
	}
	_ = syscall.Kill(-b.cmd.Process.Pid, syscall.SIGTERM)
	done := make(chan error, 1)
	go func() { done <- b.cmd.Wait() }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		_ = syscall.Kill(-b.cmd.Process.Pid, syscall.SIGKILL)
		<-done
	}
}

// read takes each message of the socket: a reply goes to its caller, and an event is handled here.
func (b *cdpBrowser) read() {
	for {
		_, data, err := b.conn.Read(b.ctx)
		if err != nil {
			return
		}
		var msg cdpMessage
		if json.Unmarshal(data, &msg) != nil {
			continue
		}
		if msg.ID != 0 {
			b.mu.Lock()
			ch := b.waiting[msg.ID]
			delete(b.waiting, msg.ID)
			b.mu.Unlock()
			if ch != nil {
				ch <- cdpReply{Result: msg.Result, Error: msg.Error}
			}
			continue
		}
		switch msg.Method {
		case "Fetch.requestPaused":
			var p struct {
				RequestID string `json:"requestId"`
				Request   struct {
					URL string `json:"url"`
				} `json:"request"`
			}
			_ = json.Unmarshal(msg.Params, &p)
			go func(session string) {
				if strings.HasPrefix(p.Request.URL, "file:") || strings.HasPrefix(p.Request.URL, "data:") {
					b.send(session, "Fetch.continueRequest", map[string]any{"requestId": p.RequestID})
					return
				}
				b.mu.Lock()
				b.blocked = append(b.blocked, p.Request.URL)
				b.mu.Unlock()
				b.send(session, "Fetch.failRequest", map[string]any{"requestId": p.RequestID, "errorReason": "BlockedByClient"})
			}(msg.SessionID)
		case "Runtime.exceptionThrown":
			b.mu.Lock()
			b.errors = append(b.errors, string(msg.Params))
			b.mu.Unlock()
		case "Runtime.consoleAPICalled":
			var p struct {
				Type string `json:"type"`
			}
			_ = json.Unmarshal(msg.Params, &p)
			if p.Type == "error" || p.Type == "assert" {
				b.mu.Lock()
				b.errors = append(b.errors, string(msg.Params))
				b.mu.Unlock()
			}
		case "Log.entryAdded":
			var p struct {
				Entry struct {
					Level string `json:"level"`
					Text  string `json:"text"`
				} `json:"entry"`
			}
			_ = json.Unmarshal(msg.Params, &p)
			if p.Entry.Level == "error" {
				b.mu.Lock()
				b.errors = append(b.errors, p.Entry.Text)
				b.mu.Unlock()
			}
		}
	}
}

// send sends a command and returns its reply channel.
func (b *cdpBrowser) send(session, method string, params any) chan cdpReply {
	b.mu.Lock()
	b.next++
	id := b.next
	ch := make(chan cdpReply, 1)
	b.waiting[id] = ch
	b.mu.Unlock()
	msg := map[string]any{"id": id, "method": method}
	if params != nil {
		msg["params"] = params
	}
	if session != "" {
		msg["sessionId"] = session
	}
	data, _ := json.Marshal(msg)
	if err := b.conn.Write(b.ctx, websocket.MessageText, data); err != nil {
		ch <- cdpReply{Error: &struct {
			Message string `json:"message"`
		}{err.Error()}}
	}
	return ch
}

// call sends a command and waits for its result.
func (b *cdpBrowser) call(session, method string, params, out any) {
	b.t.Helper()
	select {
	case r := <-b.send(session, method, params):
		if r.Error != nil {
			b.t.Fatalf("%s: %s", method, r.Error.Message)
		}
		if out != nil {
			if err := json.Unmarshal(r.Result, out); err != nil {
				b.t.Fatalf("%s: %v", method, err)
			}
		}
	case <-time.After(30 * time.Second):
		b.t.Fatalf("%s: no reply", method)
	}
}

// do sends a command to the page.
func (b *cdpBrowser) do(method string, params any) {
	b.t.Helper()
	b.call(b.session, method, params, nil)
}

// eval runs a script in the page and decodes its value into out; a thrown error fails the test.
func (b *cdpBrowser) eval(script string, out any) {
	b.t.Helper()
	var r struct {
		Result struct {
			Value json.RawMessage `json:"value"`
		} `json:"result"`
		ExceptionDetails *struct {
			Text      string `json:"text"`
			Exception struct {
				Description string `json:"description"`
			} `json:"exception"`
		} `json:"exceptionDetails"`
	}
	b.call(b.session, "Runtime.evaluate", map[string]any{"expression": script, "returnByValue": true, "awaitPromise": true}, &r)
	if r.ExceptionDetails != nil {
		b.t.Fatalf("script failed: %s %s\n%s", r.ExceptionDetails.Text, r.ExceptionDetails.Exception.Description, script)
	}
	if out != nil && len(r.Result.Value) > 0 {
		if err := json.Unmarshal(r.Result.Value, out); err != nil {
			b.t.Fatalf("script value: %v: %s", err, r.Result.Value)
		}
	}
}

// open loads the page by its file address and waits until the viewer has drawn it.
func (b *cdpBrowser) open(url string) {
	b.t.Helper()
	var ready bool
	b.do("Page.navigate", map[string]any{"url": "about:blank"})
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline) && !ready; time.Sleep(20 * time.Millisecond) {
		b.eval(`location.href === "about:blank" && document.readyState === "complete"`, &ready)
	}
	ready = false
	b.do("Page.navigate", map[string]any{"url": url})
	for deadline := time.Now().Add(15 * time.Second); time.Now().Before(deadline); time.Sleep(50 * time.Millisecond) {
		b.eval(`document.readyState === "complete" && !!document.querySelector("#nav button")`, &ready)
		if ready {
			return
		}
	}
	b.t.Fatal("the view did not render")
}

// key presses one key, with its text when it types one.
func (b *cdpBrowser) key(name string) {
	b.t.Helper()
	codes := map[string]int{"Tab": 9, "Enter": 13, "Escape": 27, " ": 32, "ArrowLeft": 37, "ArrowUp": 38, "ArrowRight": 39, "ArrowDown": 40}
	code := name
	if name == " " {
		code = "Space"
	}
	down := map[string]any{"type": "rawKeyDown", "key": name, "code": code, "windowsVirtualKeyCode": codes[name], "nativeVirtualKeyCode": codes[name]}
	if name == "Enter" {
		down["type"], down["text"] = "keyDown", "\r"
	}
	if name == " " {
		down["type"], down["text"] = "keyDown", " "
	}
	b.do("Input.dispatchKeyEvent", down)
	b.do("Input.dispatchKeyEvent", map[string]any{"type": "keyUp", "key": name, "code": code, "windowsVirtualKeyCode": codes[name], "nativeVirtualKeyCode": codes[name]})
}

// viewport sets the width of the page in CSS pixels.
func (b *cdpBrowser) viewport(width, height int) {
	b.t.Helper()
	b.do("Emulation.setDeviceMetricsOverride", map[string]any{"width": width, "height": height, "deviceScaleFactor": 1, "mobile": false})
}

// media sets the media type and the preferred colour scheme and motion of the page.
func (b *cdpBrowser) media(kind, scheme, motion string) {
	b.t.Helper()
	b.do("Emulation.setEmulatedMedia", map[string]any{"media": kind, "features": []map[string]string{
		{"name": "prefers-color-scheme", "value": scheme}, {"name": "prefers-reduced-motion", "value": motion}}})
}

// problems gives the blocked requests and the errors of the page since the last call.
func (b *cdpBrowser) problems() (blocked, errors []string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	blocked, errors = b.blocked, b.errors
	b.blocked, b.errors = nil, nil
	return
}
