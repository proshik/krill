// Package cluster joins worker hosts to the Krill Docker Swarm over SSH.
package cluster

import (
	"bytes"
	"fmt"
	"net"
	"strconv"
	"time"

	"golang.org/x/crypto/ssh"
)

// JoinSpec is everything needed to SSH into a worker and join it to the swarm.
type JoinSpec struct {
	Host        string
	Port        int
	User        string
	PrivateKey  []byte // PEM
	Token       string // swarm worker join-token (from SwarmWorkerToken)
	ManagerAddr string // "<ip>:2377"
	HostKey     string // previously accepted host key ("" = accept on first use)
}

// joinCommand is the EXACT remote command run on the worker. Only the swarm
// token (from the Docker API) and manager address (from config) are interpolated
// — never user-supplied free text — so there is no remote-command injection.
func joinCommand(token, managerAddr string) string {
	return fmt.Sprintf("docker swarm join --token %s %s", token, managerAddr)
}

// hostKeyMatches implements accept-new: an empty stored key is accepted (and the
// presented key should be persisted by the caller); a stored key must match
// exactly, else the connection is a possible MITM and is rejected.
func hostKeyMatches(stored, presented string) bool {
	return stored == "" || stored == presented
}

// sshDial builds an SSH client for spec using the given host-key callback.
// When timeout > 0 it is used as the dial timeout; otherwise 15s is applied.
func sshDial(spec JoinSpec, cb ssh.HostKeyCallback, timeout time.Duration) (*ssh.Client, error) {
	signer, err := ssh.ParsePrivateKey(spec.PrivateKey)
	if err != nil {
		return nil, fmt.Errorf("parse ssh key: %w", err)
	}
	port := spec.Port
	if port == 0 {
		port = 22
	}
	dialTimeout := 15 * time.Second
	if timeout > 0 {
		dialTimeout = timeout
	}
	cfg := &ssh.ClientConfig{
		User:            spec.User,
		Auth:            []ssh.AuthMethod{ssh.PublicKeys(signer)},
		HostKeyCallback: cb,
		Timeout:         dialTimeout,
	}
	return ssh.Dial("tcp", net.JoinHostPort(spec.Host, strconv.Itoa(port)), cfg)
}

// DialVerified opens an SSH client, verifying the presented host key strictly
// against spec.HostKey (which must already be known). spec.HostKey must be
// non-empty; an empty value is rejected so a misconfigured caller cannot
// silently accept any key. The caller closes the returned client.
func DialVerified(spec JoinSpec, timeout time.Duration) (*ssh.Client, error) {
	if spec.HostKey == "" {
		return nil, fmt.Errorf("DialVerified: host key required")
	}
	cb := func(_ string, _ net.Addr, key ssh.PublicKey) error {
		seen := string(bytes.TrimSpace(ssh.MarshalAuthorizedKey(key)))
		if !hostKeyMatches(spec.HostKey, seen) {
			return fmt.Errorf("host key mismatch for %s (possible MITM)", spec.Host)
		}
		return nil
	}
	return sshDial(spec, cb, timeout)
}

// Join SSHes into the worker, verifies/records the host key (accept-new), and
// runs the fixed swarm-join command. It returns the command's combined output
// and the presented host key (which the caller persists so later operations
// verify it). The private key and token are never logged here.
func Join(spec JoinSpec) (output, hostKey string, err error) {
	var seen string
	cb := func(_ string, _ net.Addr, key ssh.PublicKey) error {
		seen = string(bytes.TrimSpace(ssh.MarshalAuthorizedKey(key)))
		if !hostKeyMatches(spec.HostKey, seen) {
			return fmt.Errorf("host key mismatch for %s (possible MITM)", spec.Host)
		}
		return nil
	}
	cl, err := sshDial(spec, cb, 0)
	if err != nil {
		return "", seen, fmt.Errorf("ssh dial: %w", err)
	}
	defer cl.Close()
	sess, err := cl.NewSession()
	if err != nil {
		return "", seen, err
	}
	defer sess.Close()
	var buf bytes.Buffer
	sess.Stdout, sess.Stderr = &buf, &buf
	if rerr := sess.Run(joinCommand(spec.Token, spec.ManagerAddr)); rerr != nil {
		return buf.String(), seen, fmt.Errorf("swarm join: %w", rerr)
	}
	return buf.String(), seen, nil
}
