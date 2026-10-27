// Package remote runs one install command on a machine over SSH, for the
// console's "let GameFleet install it" path. The password or private key is
// used for that run only: callers keep it in memory and drop it afterwards.
//
// A fresh machine has no way to vouch for itself, so GameFleet first reads its
// host key without logging in (ReadHostKey), the operator checks it on the
// machine, and Run logs in only to the machine with that key.
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
	// HostKey is the fingerprint, SHA256:…, of the host key the operator
	// checked on the machine. Run logs in to no machine with another one.
	HostKey string
}

// HostKey is a machine's SSH host key, the way an operator checks it on the
// machine: ssh-keygen -lf File prints Fingerprint.
type HostKey struct {
	Fingerprint string
	File        string
}

// hostKeyAlgorithms is the order GameFleet takes a machine's host key types
// in, the same when it reads the key and when it logs in, so that both see
// the same key.
var hostKeyAlgorithms = []string{ssh.KeyAlgoED25519, ssh.KeyAlgoECDSA256, ssh.KeyAlgoECDSA384, ssh.KeyAlgoECDSA521,
	ssh.KeyAlgoRSASHA512, ssh.KeyAlgoRSASHA256}

// keyFile is where OpenSSH keeps a host key of the type.
func keyFile(keyType string) string {
	kind := "rsa"
	switch {
	case keyType == ssh.KeyAlgoED25519:
		kind = "ed25519"
	case strings.HasPrefix(keyType, "ecdsa-"):
		kind = "ecdsa"
	}
	return "/etc/ssh/ssh_host_" + kind + "_key.pub"
}

// errGotKey stops a connection once it has the machine's host key.
var errGotKey = errors.New("got the host key")

// ReadHostKey connects to the machine and reads its host key. It does not log
// in: the connection ends at the key, before any login is sent.
func ReadHostKey(ctx context.Context, host string, port int) (HostKey, error) {
	var got ssh.PublicKey
	config := &ssh.ClientConfig{
		User:              "gamefleet",
		HostKeyAlgorithms: hostKeyAlgorithms,
		HostKeyCallback: func(_ string, _ net.Addr, key ssh.PublicKey) error {
			got = key
			return errGotKey
		},
		Timeout: 15 * time.Second,
	}
	addr := net.JoinHostPort(host, strconv.Itoa(port))
	dialer := net.Dialer{Timeout: 15 * time.Second}
	conn, err := dialer.DialContext(ctx, "tcp", addr)
	if err != nil {
		return HostKey{}, fail("连不上 %s：%v。确认机器开着，云防火墙放行了 SSH 端口 %d", addr, err, port)
	}
	defer conn.Close()
	if deadline, ok := ctx.Deadline(); ok {
		conn.SetDeadline(deadline)
	}
	_, _, _, err = ssh.NewClientConn(conn, addr, config)
	if got == nil {
		return HostKey{}, fail("读不到 %s 的主机指纹：%v", addr, err)
	}
	return HostKey{Fingerprint: ssh.FingerprintSHA256(got), File: keyFile(got.Type())}, nil
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
		return t, fail("请填密码或者私钥")
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

// Run logs in to the machine whose host key is t.HostKey, runs command as
// root (through sudo when the user is not root), and writes everything it
// prints to out, line by line. It returns the fingerprint of the host key
// the machine showed.
func Run(ctx context.Context, t Target, command string, out io.Writer) (fingerprint string, err error) {
	t, err = t.Check()
	if err != nil {
		return "", err
	}
	if t.HostKey == "" {
		return "", fail("还没有核对过 %s 的主机指纹", t.Host)
	}
	methods, _ := t.auth()
	mismatch := false
	config := &ssh.ClientConfig{
		User:              t.User,
		Auth:              methods,
		HostKeyAlgorithms: hostKeyAlgorithms,
		// The key is checked before any login is sent: a machine with
		// another key gets neither password nor private key.
		HostKeyCallback: func(_ string, _ net.Addr, key ssh.PublicKey) error {
			fingerprint = ssh.FingerprintSHA256(key)
			if fingerprint != t.HostKey {
				mismatch = true
				return errors.New("host key mismatch")
			}
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
		if mismatch {
			return fingerprint, fail("%s 的主机指纹现在是 %s，和核对过的 %s 不一样，可能连到了别的机器。GameFleet 没有发送密码或私钥。这台机器重装过的话，请重新填一次、重新核对", addr, fingerprint, t.HostKey)
		}
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
