// Package remote runs one install command on a machine over SSH, for the
// console's "let GameFleet install it" path. The password or private key is
// used for that run only: callers keep it in memory and drop it afterwards.
package remote

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"regexp"
	"strconv"
	"strings"
	"time"

	"golang.org/x/crypto/ssh"
)

// Target is a machine and how to log in to it.
type Target struct {
	Host     string
	Port     int
	User     string
	Password string
	Key      string // a private key in PEM or OpenSSH format
	// Passphrase unlocks Key when it is encrypted.
	Passphrase string
}

// Error is a problem written for the operator. Status is the command's exit
// status when it ran and failed.
type Error struct {
	Text   string
	Status int
}

func (e *Error) Error() string { return e.Text }

func fail(format string, args ...any) error { return &Error{Text: fmt.Sprintf(format, args...)} }

var user = regexp.MustCompile(`^[a-z_][a-z0-9_-]{0,31}$`)

// Check validates the target and returns it with defaults filled in.
func (t Target) Check() (Target, error) {
	t.Host = strings.TrimSpace(t.Host)
	t.User = strings.TrimSpace(t.User)
	if t.User == "" {
		t.User = "root"
	}
	if t.Port == 0 {
		t.Port = 22
	}
	if ip := net.ParseIP(t.Host); ip == nil || ip.To4() == nil {
		return t, fail("机器地址要是 IPv4 地址，例如 203.0.113.30")
	}
	if t.Port < 1 || t.Port > 65535 {
		return t, fail("SSH 端口不对")
	}
	if !user.MatchString(t.User) {
		return t, fail("登录用户名不对")
	}
	if t.Password == "" && strings.TrimSpace(t.Key) == "" {
		return t, fail("请填 root 密码或者私钥")
	}
	if _, err := t.auth(); err != nil {
		return t, err
	}
	return t, nil
}

func (t Target) auth() ([]ssh.AuthMethod, error) {
	var methods []ssh.AuthMethod
	if key := strings.TrimSpace(t.Key); key != "" {
		var signer ssh.Signer
		var err error
		if t.Passphrase != "" {
			signer, err = ssh.ParsePrivateKeyWithPassphrase([]byte(key+"\n"), []byte(t.Passphrase))
		} else {
			signer, err = ssh.ParsePrivateKey([]byte(key + "\n"))
		}
		var missing *ssh.PassphraseMissingError
		switch {
		case errors.As(err, &missing):
			return nil, fail("这把私钥有密码保护，请一起填上私钥密码")
		case err != nil:
			return nil, fail("私钥读不出来：%v", err)
		}
		methods = append(methods, ssh.PublicKeys(signer))
	}
	if t.Password != "" {
		methods = append(methods, ssh.Password(t.Password),
			ssh.KeyboardInteractive(func(_, _ string, questions []string, _ []bool) ([]string, error) {
				answers := make([]string, len(questions))
				for i := range answers {
					answers[i] = t.Password
				}
				return answers, nil
			}))
	}
	return methods, nil
}

// Run logs in, runs command as root (through sudo when the user is not
// root), and writes everything it prints to out, line by line. It returns
// the fingerprint of the machine's host key, which a fresh machine has no
// way to vouch for in advance: the caller shows it to the operator.
func Run(ctx context.Context, t Target, command string, out io.Writer) (fingerprint string, err error) {
	t, err = t.Check()
	if err != nil {
		return "", err
	}
	methods, _ := t.auth()
	config := &ssh.ClientConfig{
		User: t.User,
		Auth: methods,
		HostKeyCallback: func(_ string, _ net.Addr, key ssh.PublicKey) error {
			fingerprint = ssh.FingerprintSHA256(key)
			return nil
		},
		Timeout: 15 * time.Second,
	}
	addr := net.JoinHostPort(t.Host, strconv.Itoa(t.Port))
	dialer := net.Dialer{Timeout: 15 * time.Second}
	conn, err := dialer.DialContext(ctx, "tcp", addr)
	if err != nil {
		return "", fail("连不上 %s：%v。确认机器开着，云防火墙放行了 SSH 端口 %d", addr, err, t.Port)
	}
	c, chans, reqs, err := ssh.NewClientConn(conn, addr, config)
	if err != nil {
		conn.Close()
		if strings.Contains(err.Error(), "unable to authenticate") {
			return fingerprint, fail("登录 %s@%s 被拒绝：密码或私钥不对", t.User, t.Host)
		}
		return fingerprint, fail("SSH 登录 %s 失败：%v", addr, err)
	}
	client := ssh.NewClient(c, chans, reqs)
	defer client.Close()
	session, err := client.NewSession()
	if err != nil {
		return fingerprint, fail("打不开 SSH 会话：%v", err)
	}
	defer session.Close()

	reader, writer := io.Pipe()
	session.Stdout, session.Stderr = writer, writer
	copied := make(chan struct{})
	go func() {
		defer close(copied)
		scanner := bufio.NewScanner(reader)
		scanner.Buffer(make([]byte, 64<<10), 1<<20)
		for scanner.Scan() {
			fmt.Fprintln(out, clean(scanner.Text()))
		}
		io.Copy(io.Discard, reader)
	}()

	if t.User != "root" {
		command = "sudo -n sh -c " + quote(command)
	}
	done := make(chan error, 1)
	go func() { done <- session.Run(command) }()
	select {
	case err = <-done:
	case <-ctx.Done():
		session.Signal(ssh.SIGTERM)
		client.Close()
		err = ctx.Err()
	}
	writer.Close()
	<-copied
	var exit *ssh.ExitError
	switch {
	case errors.As(err, &exit):
		return fingerprint, &Error{Text: fmt.Sprintf("命令在机器上失败了（退出码 %d），原因见日志", exit.ExitStatus()), Status: exit.ExitStatus()}
	case err != nil:
		return fingerprint, fail("命令没跑完：%v", err)
	}
	return fingerprint, nil
}

// quote makes s one argument for sh.
func quote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

var escape = regexp.MustCompile(`\x1b\[[0-9;?]*[A-Za-z]`)

// clean drops colours and carriage returns from one line of output.
func clean(line string) string {
	line = escape.ReplaceAllString(line, "")
	if i := strings.LastIndex(line, "\r"); i >= 0 {
		line = line[i+1:]
	}
	return line
}
