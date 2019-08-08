// Sshwifty - A Web SSH client
//
// Copyright (C) 2019 Rui NI <nirui@gmx.com>
//
// This program is free software: you can redistribute it and/or modify
// it under the terms of the GNU Affero General Public License as
// published by the Free Software Foundation, either version 3 of the
// License, or (at your option) any later version.
//
// This program is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
// GNU Affero General Public License for more details.
//
// You should have received a copy of the GNU Affero General Public License
// along with this program.  If not, see <https://www.gnu.org/licenses/>.

package commands

import (
	"errors"
	"io"
	"net"
	"sync"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/niruix/sshwifty/application/command"
	"github.com/niruix/sshwifty/application/log"
	"github.com/niruix/sshwifty/application/network"
	"github.com/niruix/sshwifty/application/rw"
)

// Server -> client signal Consts
const (
	SSHServerRemoteStdOut             = 0x00
	SSHServerRemoteStdErr             = 0x01
	SSHServerConnectFailed            = 0x02
	SSHServerConnectSucceed           = 0x03
	SSHServerConnectVerifyFingerprint = 0x04
	SSHServerConnectRequestCredential = 0x05
)

// Client -> server signal consts
const (
	SSHClientStdIn              = 0x00
	SSHClientResize             = 0x01
	SSHClientRespondFingerprint = 0x02
	SSHClientRespondCredential  = 0x03
)

const (
	sshCredentialMaxSize = 4096
)

// Error codes
const (
	SSHRequestErrorBadUserName      = command.StreamError(0x01)
	SSHRequestErrorBadRemoteAddress = command.StreamError(0x02)
	SSHRequestErrorBadAuthMethod    = command.StreamError(0x03)
)

// Auth methods
const (
	SSHAuthMethodNone       byte = 0x00
	SSHAuthMethodPasspharse byte = 0x01
	SSHAuthMethodPrivateKey byte = 0x02
)

type sshAuthMethodBuilder func(b []byte) []ssh.AuthMethod

// Errors
var (
	ErrSSHAuthCancelled = errors.New(
		"Authenication has been cancelled")

	ErrSSHInvalidAuthMethod = errors.New(
		"Invalid auth method")

	ErrSSHInvalidAddress = errors.New(
		"Invalid address")

	ErrSSHRemoteFingerprintVerificationCancelled = errors.New(
		"Server Fingerprint verification process has been cancelled")

	ErrSSHRemoteFingerprintRefused = errors.New(
		"Server Fingerprint has been refused")

	ErrSSHRemoteConnUnavailable = errors.New(
		"Remote SSH connection is unavailable")

	ErrSSHUnexpectedFingerprintVerificationRespond = errors.New(
		"Unexpected fingerprint verification respond")

	ErrSSHUnexpectedCredentialDataRespond = errors.New(
		"Unexpected credential data respond")

	ErrSSHCredentialDataTooLarge = errors.New(
		"Credential was too large")

	ErrSSHUnknownClientSignal = errors.New(
		"Unknown client signal")
)

type sshRemoteConn struct {
	writer  io.Writer
	closer  func() error
	session *ssh.Session
}

func (s sshRemoteConn) isValid() bool {
	return s.writer != nil && s.closer != nil && s.session != nil
}

type sshClient struct {
	w                                    command.StreamResponder
	l                                    log.Logger
	dial                                 network.Dial
	dialTimeout                          time.Duration
	remoteCloseWait                      sync.WaitGroup
	credentialReceive                    chan []byte
	credentialProcessed                  bool
	credentialReceiveClosed              bool
	fingerprintVerifyResultReceive       chan bool
	fingerprintProcessed                 bool
	fingerprintVerifyResultReceiveClosed bool
	remoteConnReceive                    chan sshRemoteConn
	remoteConn                           sshRemoteConn
}

func newSSH(
	l log.Logger,
	w command.StreamResponder,
	dial network.Dial,
) command.FSMMachine {
	return &sshClient{
		w:                                    w,
		l:                                    l,
		dial:                                 dial,
		dialTimeout:                          10 * time.Second,
		remoteCloseWait:                      sync.WaitGroup{},
		credentialReceive:                    make(chan []byte, 1),
		credentialProcessed:                  false,
		credentialReceiveClosed:              false,
		fingerprintVerifyResultReceive:       make(chan bool, 1),
		fingerprintProcessed:                 false,
		fingerprintVerifyResultReceiveClosed: false,
		remoteConnReceive:                    make(chan sshRemoteConn, 1),
		remoteConn:                           sshRemoteConn{},
	}
}

func (d *sshClient) Bootup(
	r *rw.LimitedReader,
	b []byte,
) (command.FSMState, command.FSMError) {
	// User name
	userName, userNameErr := ParseString(r.Read, b)

	if userNameErr != nil {
		return nil, command.ToFSMError(
			userNameErr, SSHRequestErrorBadUserName)
	}

	userNameStr := string(userName.Data())

	// Address
	addr, addrErr := ParseAddress(r.Read, b)

	if addrErr != nil {
		return nil, command.ToFSMError(
			addrErr, SSHRequestErrorBadRemoteAddress)
	}

	addrStr := addr.String()

	if len(addrStr) <= 0 {
		return nil, command.ToFSMError(
			ErrSSHInvalidAddress, SSHRequestErrorBadRemoteAddress)
	}

	// Auth method
	rData, rErr := rw.FetchOneByte(r.Fetch)

	if rErr != nil {
		return nil, command.ToFSMError(
			rErr, SSHRequestErrorBadAuthMethod)
	}

	authMethodBuilder, authMethodBuilderErr := d.buildAuthMethod(rData[0])

	if authMethodBuilderErr != nil {
		return nil, command.ToFSMError(
			authMethodBuilderErr, SSHRequestErrorBadAuthMethod)
	}

	d.remoteCloseWait.Add(1)
	go d.remote(userNameStr, addrStr, authMethodBuilder)

	return d.local, command.NoFSMError()
}

func (d *sshClient) buildAuthMethod(
	methodType byte) (sshAuthMethodBuilder, error) {
	switch methodType {
	case SSHAuthMethodNone:
		return func(b []byte) []ssh.AuthMethod {
			return nil
		}, nil

	case SSHAuthMethodPasspharse:
		return func(b []byte) []ssh.AuthMethod {
			return []ssh.AuthMethod{
				ssh.PasswordCallback(func() (string, error) {
					wErr := d.w.SendManual(
						SSHServerConnectRequestCredential,
						b[d.w.HeaderSize():],
					)

					if wErr != nil {
						return "", wErr
					}

					passpharseBytes, passpharseReceived := <-d.credentialReceive

					if !passpharseReceived {
						return "", ErrSSHAuthCancelled
					}

					return string(passpharseBytes), nil
				}),
			}
		}, nil

	case SSHAuthMethodPrivateKey:
		return func(b []byte) []ssh.AuthMethod {
			return []ssh.AuthMethod{
				ssh.PublicKeysCallback(func() ([]ssh.Signer, error) {
					wErr := d.w.SendManual(
						SSHServerConnectRequestCredential,
						b[d.w.HeaderSize():],
					)

					if wErr != nil {
						return nil, wErr
					}

					privateKeyBytes, privateKeyReceived := <-d.credentialReceive

					if !privateKeyReceived {
						return nil, ErrSSHAuthCancelled
					}

					signer, signerErr := ssh.ParsePrivateKey(privateKeyBytes)

					if signerErr != nil {
						return nil, signerErr
					}

					return []ssh.Signer{signer}, signerErr
				}),
			}
		}, nil
	}

	return nil, ErrSSHInvalidAuthMethod
}

func (d *sshClient) comfirmRemoteFingerprint(
	hostname string,
	remote net.Addr,
	key ssh.PublicKey,
	buf []byte,
) error {
	fgp := ssh.FingerprintSHA256(key)
	fgpLen := copy(buf[d.w.HeaderSize():], fgp)

	wErr := d.w.SendManual(
		SSHServerConnectVerifyFingerprint,
		buf[:d.w.HeaderSize()+fgpLen],
	)

	if wErr != nil {
		return wErr
	}

	confirmed, confirmOK := <-d.fingerprintVerifyResultReceive

	if !confirmOK {
		return ErrSSHRemoteFingerprintVerificationCancelled
	}

	if !confirmed {
		return ErrSSHRemoteFingerprintRefused
	}

	return nil
}

func (d *sshClient) dialRemote(
	network, addr string, config *ssh.ClientConfig) (*ssh.Client, error) {
	conn, err := d.dial(network, addr, config.Timeout)

	if err != nil {
		return nil, err
	}

	c, chans, reqs, err := ssh.NewClientConn(conn, addr, config)

	if err != nil {
		return nil, err
	}

	return ssh.NewClient(c, chans, reqs), nil
}

func (d *sshClient) remote(
	user string, address string, authMethodBuilder sshAuthMethodBuilder) {
	defer func() {
		d.w.Signal(command.HeaderClose)

		close(d.remoteConnReceive)
		d.remoteCloseWait.Done()
	}()

	buf := [4096]byte{}

	conn, dErr := d.dialRemote("tcp", address, &ssh.ClientConfig{
		User: user,
		Auth: authMethodBuilder(buf[:]),
		HostKeyCallback: func(h string, r net.Addr, k ssh.PublicKey) error {
			return d.comfirmRemoteFingerprint(h, r, k, buf[:])
		},
		Timeout: d.dialTimeout,
	})

	if dErr != nil {
		errLen := copy(buf[d.w.HeaderSize():], dErr.Error()) + d.w.HeaderSize()
		d.w.SendManual(SSHServerConnectFailed, buf[:errLen])

		d.l.Debug("Unable to connect to remote machine: %s", dErr)

		return
	}

	defer conn.Close()

	session, sErr := conn.NewSession()

	if sErr != nil {
		errLen := copy(buf[d.w.HeaderSize():], sErr.Error()) + d.w.HeaderSize()
		d.w.SendManual(SSHServerConnectFailed, buf[:errLen])

		d.l.Debug("Unable open new session on remote machine: %s", sErr)

		return
	}

	defer session.Close()

	in, inErr := session.StdinPipe()

	if inErr != nil {
		errLen := copy(buf[d.w.HeaderSize():], inErr.Error()) + d.w.HeaderSize()
		d.w.SendManual(SSHServerConnectFailed, buf[:errLen])

		d.l.Debug("Unable export Stdin pipe: %s", inErr)

		return
	}

	out, outErr := session.StdoutPipe()

	if outErr != nil {
		errLen := copy(buf[d.w.HeaderSize():], outErr.Error()) +
			d.w.HeaderSize()
		d.w.SendManual(SSHServerConnectFailed, buf[:errLen])

		d.l.Debug("Unable export Stdout pipe: %s", outErr)

		return
	}

	errOut, outErrErr := session.StderrPipe()

	if outErrErr != nil {
		errLen := copy(buf[d.w.HeaderSize():], outErrErr.Error()) +
			d.w.HeaderSize()
		d.w.SendManual(SSHServerConnectFailed, buf[:errLen])

		d.l.Debug("Unable export Stderr pipe: %s", outErrErr)

		return
	}

	sErr = session.RequestPty("xterm", 80, 40, ssh.TerminalModes{
		ssh.ECHO:          1,
		ssh.TTY_OP_ISPEED: 14400,
		ssh.TTY_OP_OSPEED: 14400,
	})

	if sErr != nil {
		errLen := copy(buf[d.w.HeaderSize():], sErr.Error()) + d.w.HeaderSize()
		d.w.SendManual(SSHServerConnectFailed, buf[:errLen])

		d.l.Debug("Unable request PTY: %s", sErr)

		return
	}

	sErr = session.Shell()

	if sErr != nil {
		errLen := copy(buf[d.w.HeaderSize():], sErr.Error()) + d.w.HeaderSize()
		d.w.SendManual(SSHServerConnectFailed, buf[:errLen])

		d.l.Debug("Unable to start Shell: %s", sErr)

		return
	}

	defer session.Wait()

	d.remoteConnReceive <- sshRemoteConn{
		writer: in,
		closer: func() error {
			sErr := session.Close()

			if sErr != nil {
				return sErr
			}

			return conn.Close()
		},
		session: session,
	}

	wErr := d.w.SendManual(
		SSHServerConnectSucceed, buf[:d.w.HeaderSize()])

	if wErr != nil {
		return
	}

	d.l.Debug("Serving")

	d.remoteCloseWait.Add(1)

	go func() {
		defer d.remoteCloseWait.Done()

		errOutBuf := [4096]byte{}

		for {
			rLen, rErr := errOut.Read(errOutBuf[d.w.HeaderSize():])

			if rErr != nil {
				return
			}

			rErr = d.w.SendManual(
				SSHServerRemoteStdErr, errOutBuf[:d.w.HeaderSize()+rLen])

			if rErr != nil {
				return
			}
		}
	}()

	for {
		rLen, rErr := out.Read(buf[d.w.HeaderSize():])

		if rErr != nil {
			return
		}

		rErr = d.w.SendManual(
			SSHServerRemoteStdOut, buf[:d.w.HeaderSize()+rLen])

		if rErr != nil {
			return
		}
	}
}

func (d *sshClient) getRemote() (sshRemoteConn, error) {
	if d.remoteConn.isValid() {
		return d.remoteConn, nil
	}

	remoteConn, remoteConnFetched := <-d.remoteConnReceive

	if !remoteConnFetched {
		return sshRemoteConn{}, ErrSSHRemoteConnUnavailable
	}

	d.remoteConn = remoteConn

	return d.remoteConn, nil
}

func (d *sshClient) local(
	f *command.FSM,
	r *rw.LimitedReader,
	h command.StreamHeader,
	b []byte,
) error {
	switch h.Marker() {
	case SSHClientStdIn:
		remote, remoteErr := d.getRemote()

		if remoteErr != nil {
			return remoteErr
		}

		for !r.Completed() {
			rData, rErr := r.Buffered()

			if rErr != nil {
				return rErr
			}

			_, wErr := remote.writer.Write(rData)

			if wErr != nil {
				return wErr
			}
		}

		return nil

	case SSHClientResize:
		remote, remoteErr := d.getRemote()

		if remoteErr != nil {
			return remoteErr
		}

		_, rErr := io.ReadFull(r, b[:4])

		if rErr != nil {
			return rErr
		}

		rows := int(b[0])
		rows <<= 8
		rows |= int(b[1])

		cols := int(b[2])
		cols <<= 8
		cols |= int(b[3])

		// It's ok for it to fail
		wcErr := remote.session.WindowChange(rows, cols)

		if wcErr != nil {
			d.l.Debug("Failed to resize to %d, %d: %s", rows, cols, wcErr)
		}

		return nil

	case SSHClientRespondFingerprint:
		if d.fingerprintProcessed {
			return ErrSSHUnexpectedFingerprintVerificationRespond
		}

		d.fingerprintProcessed = true

		rData, rErr := rw.FetchOneByte(r.Fetch)

		if rErr != nil {
			return rErr
		}

		comfirmed := rData[0] == 0

		if !comfirmed {
			d.fingerprintVerifyResultReceive <- false

			remote, remoteErr := d.getRemote()

			if remoteErr == nil {
				remote.closer()
			}
		} else {
			d.fingerprintVerifyResultReceive <- true
		}

		return nil

	case SSHClientRespondCredential:
		if d.credentialProcessed {
			return ErrSSHUnexpectedCredentialDataRespond
		}

		d.credentialProcessed = true

		sshCredentialBufSize := 0

		if r.Remains() > sshCredentialMaxSize {
			sshCredentialBufSize = sshCredentialMaxSize
		} else {
			sshCredentialBufSize = r.Remains()
		}

		credentialDataBuf := make([]byte, 0, sshCredentialBufSize)
		totalCredentialRead := 0

		for !r.Completed() {
			rData, rErr := r.Buffered()

			if rErr != nil {
				return rErr
			}

			totalCredentialRead += len(rData)

			if totalCredentialRead > sshCredentialBufSize {
				return ErrSSHCredentialDataTooLarge
			}

			credentialDataBuf = append(credentialDataBuf, rData...)
		}

		d.credentialReceive <- credentialDataBuf

		return nil

	default:
		return ErrSSHUnknownClientSignal
	}
}

func (d *sshClient) Close() error {
	d.credentialProcessed = true
	d.fingerprintProcessed = true

	if !d.credentialReceiveClosed {
		close(d.credentialReceive)

		d.credentialReceiveClosed = true
	}

	if !d.fingerprintVerifyResultReceiveClosed {
		close(d.fingerprintVerifyResultReceive)

		d.fingerprintVerifyResultReceiveClosed = true
	}

	remote, remoteErr := d.getRemote()

	if remoteErr == nil {
		remote.closer()
	}

	d.remoteCloseWait.Wait()

	return nil
}

func (d *sshClient) Release() error {
	return nil
}