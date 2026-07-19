package daemon

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"time"
)

// Hello is the one-shot line the daemon emits on every new connection.
type Hello struct {
	Codegraph  string `json:"codegraph"`
	PID        int    `json:"pid"`
	SocketPath string `json:"socketPath"`
	Protocol   int    `json:"protocol"`
}

// ClientHello is the optional reverse handshake from proxy → daemon.
type ClientHello struct {
	CodegraphClient int  `json:"codegraph_client"`
	PID             int  `json:"pid"`
	HostPID         *int `json:"hostPid"`
}

const (
	maxHelloLineBytes    = 4096
	clientHelloTimeout   = 3 * time.Second
	handshakeProtocol = 1
)

// WriteHello sends the daemon hello line.
func WriteHello(w io.Writer, socketPath string) error {
	h := Hello{
		Codegraph:  PackageVersion,
		PID:        os.Getpid(),
		SocketPath: socketPath,
		Protocol:   handshakeProtocol,
	}
	b, err := json.Marshal(h)
	if err != nil {
		return err
	}
	_, err = w.Write(append(b, '\n'))
	return err
}

// ReadHello reads one NDJSON line and parses it as Hello.
func ReadHello(r *bufio.Reader) (Hello, error) {
	line, err := readLineLimited(r, maxHelloLineBytes)
	if err != nil {
		return Hello{}, err
	}
	var h Hello
	if err := json.Unmarshal(line, &h); err != nil {
		return Hello{}, fmt.Errorf("bad daemon hello: %w", err)
	}
	if h.Codegraph == "" || h.Protocol != handshakeProtocol {
		return Hello{}, fmt.Errorf("unsupported daemon hello")
	}
	return h, nil
}

// WriteClientHello sends the optional proxy identity line.
func WriteClientHello(w io.Writer) error {
	ch := ClientHello{
		CodegraphClient: 1,
		PID:             os.Getpid(),
	}
	b, err := json.Marshal(ch)
	if err != nil {
		return err
	}
	_, err = w.Write(append(b, '\n'))
	return err
}

// TryReadClientHello waits briefly (via conn read deadline) for a client-hello.
// Non-hello first lines are returned as leftover so the MCP session can consume them.
func TryReadClientHello(conn net.Conn, r *bufio.Reader) (peers ClientHello, leftover []byte, ok bool) {
	_ = conn.SetReadDeadline(time.Now().Add(clientHelloTimeout))
	defer conn.SetReadDeadline(time.Time{}) // clear

	line, err := readLineLimited(r, maxHelloLineBytes)
	if err != nil {
		return ClientHello{}, nil, false
	}
	var chlo ClientHello
	if err := json.Unmarshal(line, &chlo); err != nil || chlo.CodegraphClient != 1 {
		left := append(append([]byte{}, line...), '\n')
		return ClientHello{}, left, false
	}
	return chlo, nil, true
}

func readLineLimited(r *bufio.Reader, max int) ([]byte, error) {
	var buf []byte
	for {
		part, isPrefix, err := r.ReadLine()
		if err != nil {
			return nil, err
		}
		buf = append(buf, part...)
		if len(buf) > max {
			return nil, fmt.Errorf("hello line too long")
		}
		if !isPrefix {
			return buf, nil
		}
	}
}
