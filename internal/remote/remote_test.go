package remote

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/binary"
	"encoding/pem"
	"net"
	"strings"
	"testing"

	"golang.org/x/crypto/ssh"
)

// sshServer accepts the password or the client key, runs nothing, prints the
// command it got and exits with status 3 when the command contains "fail".
func sshServer(t *testing.T, password string, clientKey ssh.PublicKey) (port int, hostKey ssh.PublicKey) {
	t.Helper()
	_, private, _ := ed25519.GenerateKey(rand.Reader)
	signer, _ := ssh.NewSignerFromKey(private)
	config := &ssh.ServerConfig{
		PasswordCallback: func(_ ssh.ConnMetadata, p []byte) (*ssh.Permissions, error) {
			if string(p) == password {
				return nil, nil
			}
			return nil, ssh.ErrNoAuth
		},
		PublicKeyCallback: func(_ ssh.ConnMetadata, key ssh.PublicKey) (*ssh.Permissions, error) {
			if clientKey != nil && bytes.Equal(key.Marshal(), clientKey.Marshal()) {
				return nil, nil
			}
			return nil, ssh.ErrNoAuth
		},
	}
	config.AddHostKey(signer)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { listener.Close() })
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			go serve(conn, config)
		}
	}()
	return listener.Addr().(*net.TCPAddr).Port, signer.PublicKey()
}

func serve(conn net.Conn, config *ssh.ServerConfig) {
	_, chans, reqs, err := ssh.NewServerConn(conn, config)
	if err != nil {
		return
	}
	go ssh.DiscardRequests(reqs)
	for nc := range chans {
		ch, requests, _ := nc.Accept()
		go func() {
			for req := range requests {
				if req.Type != "exec" {
					req.Reply(false, nil)
					continue
				}
				command := string(req.Payload[4:])
				req.Reply(true, nil)
				ch.Write([]byte("\x1b[32mstarting\x1b[0m\r\nran: " + command + "\n"))
				status := make([]byte, 4)
				if strings.Contains(command, "fail") {
					binary.BigEndian.PutUint32(status, 3)
				}
				ch.SendRequest("exit-status", false, status)
				ch.Close()
			}
		}()
	}
}

func TestRunWithAPassword(t *testing.T) {
	port, hostKey := sshServer(t, "hunter2", nil)
	var out bytes.Buffer
	fp, err := Run(context.Background(), Target{Host: "127.0.0.1", Port: port, Password: "hunter2"}, "echo hi", &out)
	if err != nil {
		t.Fatal(err)
	}
	if fp != ssh.FingerprintSHA256(hostKey) {
		t.Errorf("fingerprint %s", fp)
	}
	if out.String() != "starting\nran: echo hi\n" {
		t.Errorf("output %q", out.String())
	}
	out.Reset()
	_, err = Run(context.Background(), Target{Host: "127.0.0.1", Port: port, Password: "hunter2"}, "fail now", &out)
	if err == nil || !strings.Contains(err.Error(), "退出码 3") {
		t.Errorf("failing command: %v", err)
	}
	_, err = Run(context.Background(), Target{Host: "127.0.0.1", Port: port, Password: "wrong"}, "echo", &out)
	if err == nil || !strings.Contains(err.Error(), "密码或私钥不对") {
		t.Errorf("wrong password: %v", err)
	}
}

func TestRunWithAKeyAndSudo(t *testing.T) {
	public, private, _ := ed25519.GenerateKey(rand.Reader)
	sshPublic, _ := ssh.NewPublicKey(public)
	block, _ := ssh.MarshalPrivateKey(private, "")
	port, _ := sshServer(t, "", sshPublic)
	var out bytes.Buffer
	target := Target{Host: "127.0.0.1", Port: port, User: "debian", Key: string(pem.EncodeToMemory(block))}
	if _, err := Run(context.Background(), target, "install 'it'", &out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), `ran: sudo -n sh -c 'install '\''it'\'''`) {
		t.Errorf("output %q", out.String())
	}
}

func TestCheck(t *testing.T) {
	encrypted, _ := ssh.MarshalPrivateKeyWithPassphrase(ed25519.NewKeyFromSeed(make([]byte, 32)), "", []byte("secret"))
	cases := map[string]Target{
		"no address":     {Password: "x"},
		"hostname":       {Host: "example.com", Password: "x"},
		"no credentials": {Host: "203.0.113.7"},
		"bad user":       {Host: "203.0.113.7", User: "Root!", Password: "x"},
		"bad key":        {Host: "203.0.113.7", Key: "not a key"},
		"no passphrase":  {Host: "203.0.113.7", Key: string(pem.EncodeToMemory(encrypted))},
		"bad port":       {Host: "203.0.113.7", Port: 70000, Password: "x"},
	}
	for name, target := range cases {
		if _, err := target.Check(); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
	good, err := Target{Host: " 203.0.113.7 ", Password: "x"}.Check()
	if err != nil || good.User != "root" || good.Port != 22 || good.Host != "203.0.113.7" {
		t.Errorf("defaults: %+v, %v", good, err)
	}
	withPassphrase := Target{Host: "203.0.113.7", Key: string(pem.EncodeToMemory(encrypted)), Passphrase: "secret"}
	if _, err := withPassphrase.Check(); err != nil {
		t.Errorf("key with passphrase: %v", err)
	}
}
