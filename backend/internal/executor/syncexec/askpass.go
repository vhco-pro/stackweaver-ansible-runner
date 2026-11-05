// Copyright (c) 2026 VH & Co BV. Licensed under the Business Source License 1.1. See LICENSE for details.

package syncexec

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// EnvAskpassSocket names the socket a git askpass helper asks for a credential on. The runner
// binaries check it first thing: started by git as GIT_ASKPASS with it set, the binary is the
// helper, answers one prompt and exits (AskpassMain).
const EnvAskpassSocket = "STACKWEAVER_ASKPASS_SOCKET"

// IsAskpass reports whether this process was started by git as the askpass helper.
func IsAskpass() bool { return os.Getenv(EnvAskpassSocket) != "" }

// AskpassMain is the askpass helper: it sends git's prompt (its one argument) to the executor's
// socket, prints the answer for git and returns the exit code. The credential passes from the
// executor's memory to git through the socket and git's pipe from this process, never through an
// argument, an environment variable or a file (decision 18, AC68).
func AskpassMain(args []string) int {
	prompt := ""
	if len(args) > 0 {
		prompt = args[0]
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, err := (&net.Dialer{}).DialContext(ctx, "unix", os.Getenv(EnvAskpassSocket)) //nolint:gosec // G704: the executor's own socket, named by the executor
	if err != nil {
		fmt.Fprintln(os.Stderr, "askpass: the executor is not answering:", err)
		return 1
	}
	defer func() { _ = conn.Close() }()
	_ = conn.SetDeadline(time.Now().Add(10 * time.Second))
	if _, err := fmt.Fprintln(conn, strings.ReplaceAll(prompt, "\n", " ")); err != nil {
		return 1
	}
	answer, err := io.ReadAll(io.LimitReader(conn, 64<<10))
	if err != nil {
		return 1
	}
	if _, err := os.Stdout.Write(append(answer, '\n')); err != nil {
		return 1
	}
	return 0
}

// askpassServer answers the askpass helper's prompts for one clone: the user name once and the
// token once. It is the one-shot helper the credential reaches git through.
type askpassServer struct {
	ln       net.Listener
	path     string
	own      string // a directory created for the socket, removed with it
	username string
	token    string

	mu                 sync.Mutex
	gaveUser, gavePass bool
	done               chan struct{}
}

// maxSocketPath stays under the 108 bytes a unix socket path may have.
const maxSocketPath = 100

// serveAskpass listens on a socket in dir, which must be the job's private secret directory. A dir
// whose path is too long for a socket gets a private 0700 directory of its own instead.
func serveAskpass(ctx context.Context, dir, username, token string) (*askpassServer, error) {
	path := filepath.Join(dir, "askpass.sock")
	own := ""
	if len(path) > maxSocketPath {
		d, err := os.MkdirTemp("", "sw-askpass-")
		if err != nil {
			return nil, fmt.Errorf("creating the askpass socket directory: %w", err)
		}
		own, path = d, filepath.Join(d, "s")
	}
	ln, err := (&net.ListenConfig{}).Listen(ctx, "unix", path)
	if err != nil {
		if own != "" {
			_ = os.RemoveAll(own)
		}
		return nil, fmt.Errorf("listening for the askpass helper: %w", err)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		_ = ln.Close()
		return nil, fmt.Errorf("restricting the askpass socket: %w", err)
	}
	s := &askpassServer{ln: ln, path: path, own: own, username: username, token: token, done: make(chan struct{})}
	go s.serve()
	return s, nil
}

func (s *askpassServer) serve() {
	defer close(s.done)
	for {
		conn, err := s.ln.Accept()
		if err != nil {
			return
		}
		if s.answer(conn) {
			_ = s.ln.Close()
			return
		}
	}
}

// answer answers one prompt and reports whether both credentials have been handed out, after
// which the server stops.
func (s *askpassServer) answer(conn net.Conn) bool {
	defer func() { _ = conn.Close() }()
	_ = conn.SetDeadline(time.Now().Add(10 * time.Second))
	prompt, err := bufio.NewReader(conn).ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	lower := strings.ToLower(prompt)
	switch {
	case strings.HasPrefix(lower, "username") && !s.gaveUser:
		s.gaveUser = true
		_, _ = io.WriteString(conn, s.username)
	case strings.HasPrefix(lower, "password") && !s.gavePass:
		s.gavePass = true
		_, _ = io.WriteString(conn, s.token)
	}
	return s.gavePass && (s.gaveUser || s.username == "")
}

// Close stops answering and removes the socket.
func (s *askpassServer) Close() {
	_ = s.ln.Close()
	<-s.done
	_ = os.Remove(s.path)
	if s.own != "" {
		_ = os.RemoveAll(s.own)
	}
}
