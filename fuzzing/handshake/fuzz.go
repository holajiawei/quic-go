package handshake

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"log"

	"github.com/lucas-clemente/quic-go/fuzzing/internal/helper"
	"github.com/lucas-clemente/quic-go/internal/handshake"
	"github.com/lucas-clemente/quic-go/internal/protocol"
	"github.com/lucas-clemente/quic-go/internal/utils"
	"github.com/lucas-clemente/quic-go/internal/wire"
)

var cert *tls.Certificate
var certPool *x509.CertPool

func init() {
	priv, err := rsa.GenerateKey(rand.Reader, 1024)
	if err != nil {
		log.Fatal(err)
	}
	cert, certPool, err = helper.GenerateCertificate(priv)
	if err != nil {
		log.Fatal(err)
	}
}

type messageType uint8

// TLS handshake message types.
const (
	typeClientHello         messageType = 1
	typeServerHello         messageType = 2
	typeNewSessionTicket    messageType = 4
	typeEncryptedExtensions messageType = 8
	typeCertificate         messageType = 11
	typeCertificateRequest  messageType = 13
	typeCertificateVerify   messageType = 15
	typeFinished            messageType = 20
)

func (m messageType) String() string {
	switch m {
	case typeClientHello:
		return "ClientHello"
	case typeServerHello:
		return "ServerHello"
	case typeNewSessionTicket:
		return "NewSessionTicket"
	case typeEncryptedExtensions:
		return "EncryptedExtensions"
	case typeCertificate:
		return "Certificate"
	case typeCertificateRequest:
		return "CertificateRequest"
	case typeCertificateVerify:
		return "CertificateVerify"
	case typeFinished:
		return "Finished"
	default:
		return fmt.Sprintf("unknown message type: %d", m)
	}
}

type chunk struct {
	data     []byte
	encLevel protocol.EncryptionLevel
}

type stream struct {
	chunkChan chan<- chunk
	encLevel  protocol.EncryptionLevel
}

func (s *stream) Write(b []byte) (int, error) {
	data := append([]byte{}, b...)
	select {
	case s.chunkChan <- chunk{data: data, encLevel: s.encLevel}:
	default:
		panic("chunkChan too small")
	}
	return len(b), nil
}

func initStreams() (chan chunk, *stream /* initial */, *stream /* handshake */) {
	chunkChan := make(chan chunk, 10)
	initialStream := &stream{chunkChan: chunkChan, encLevel: protocol.EncryptionInitial}
	handshakeStream := &stream{chunkChan: chunkChan, encLevel: protocol.EncryptionHandshake}
	return chunkChan, initialStream, handshakeStream
}

type handshakeRunner interface {
	OnReceivedParams(*wire.TransportParameters)
	OnHandshakeComplete()
	OnError(error)
	DropKeys(protocol.EncryptionLevel)
}

type runner struct {
	errored        bool
	client, server *handshake.CryptoSetup
}

var _ handshakeRunner = &runner{}

func newRunner(client, server *handshake.CryptoSetup) *runner {
	return &runner{client: client, server: server}
}

func (r *runner) OnReceivedParams(*wire.TransportParameters) {}
func (r *runner) OnHandshakeComplete()                       {}
func (r *runner) OnError(err error) {
	if r.errored {
		return
	}
	r.errored = true
	(*r.client).Close()
	(*r.server).Close()
}
func (r *runner) DropKeys(protocol.EncryptionLevel) {}

const alpn = "fuzzing"

func toEncryptionLevel(n uint8) protocol.EncryptionLevel {
	switch n % 3 {
	default:
		return protocol.EncryptionInitial
	case 1:
		return protocol.EncryptionHandshake
	case 2:
		return protocol.Encryption1RTT
	}
}

func maxEncLevel(cs handshake.CryptoSetup, encLevel protocol.EncryptionLevel) protocol.EncryptionLevel {
	switch encLevel {
	case protocol.EncryptionInitial:
		return protocol.EncryptionInitial
	case protocol.EncryptionHandshake:
		// Handshake opener not available. We can't possibly read a Handshake handshake message.
		if opener, err := cs.GetHandshakeOpener(); err != nil || opener == nil {
			return protocol.EncryptionInitial
		}
		return protocol.EncryptionHandshake
	case protocol.Encryption1RTT:
		// 1-RTT opener not available. We can't possibly read a post-handshake message.
		if opener, err := cs.Get1RTTOpener(); err != nil || opener == nil {
			return maxEncLevel(cs, protocol.EncryptionHandshake)
		}
		return protocol.Encryption1RTT
	default:
		panic("unexpected encryption level")
	}
}

// PrefixLen is the number of bytes used for configuration
const PrefixLen = 2

// Fuzz fuzzes the TLS 1.3 handshake used by QUIC.
//go:generate go run ./cmd/corpus.go
func Fuzz(data []byte) int {
	if len(data) < PrefixLen {
		return -1
	}
	enable0RTTClient := helper.NthBit(data[0], 0)
	enable0RTTServer := helper.NthBit(data[0], 1)
	useSessionTicketCache := helper.NthBit(data[0], 2)
	sendPostHandshakeMessageToClient := helper.NthBit(data[0], 3)
	sendPostHandshakeMessageToServer := helper.NthBit(data[0], 4)
	messageToReplace := data[1] % 32
	messageToReplaceEncLevel := toEncryptionLevel(data[1] >> 6)
	data = data[PrefixLen:]

	clientConf := &tls.Config{
		ServerName: "localhost",
		NextProtos: []string{alpn},
		RootCAs:    certPool,
	}
	if useSessionTicketCache {
		clientConf.ClientSessionCache = tls.NewLRUClientSessionCache(5)
	}
	cChunkChan, cInitialStream, cHandshakeStream := initStreams()
	var client, server handshake.CryptoSetup
	runner := newRunner(&client, &server)
	client, _ = handshake.NewCryptoSetupClient(
		cInitialStream,
		cHandshakeStream,
		protocol.ConnectionID{},
		nil,
		nil,
		&wire.TransportParameters{},
		runner,
		clientConf,
		enable0RTTClient,
		utils.NewRTTStats(),
		nil,
		utils.DefaultLogger.WithPrefix("client"),
	)

	sChunkChan, sInitialStream, sHandshakeStream := initStreams()
	server = handshake.NewCryptoSetupServer(
		sInitialStream,
		sHandshakeStream,
		protocol.ConnectionID{},
		nil,
		nil,
		&wire.TransportParameters{},
		runner,
		&tls.Config{
			Certificates: []tls.Certificate{*cert},
			NextProtos:   []string{alpn},
		},
		enable0RTTServer,
		utils.NewRTTStats(),
		nil,
		utils.DefaultLogger.WithPrefix("server"),
	)

	if len(data) == 0 {
		return -1
	}

	serverHandshakeCompleted := make(chan struct{})
	go func() {
		defer close(serverHandshakeCompleted)
		server.RunHandshake()
	}()

	clientHandshakeCompleted := make(chan struct{})
	go func() {
		defer close(clientHandshakeCompleted)
		client.RunHandshake()
	}()

	done := make(chan struct{})
	go func() {
		<-serverHandshakeCompleted
		<-clientHandshakeCompleted
		close(done)
	}()

messageLoop:
	for {
		select {
		case c := <-cChunkChan:
			b := c.data
			encLevel := c.encLevel
			if len(b) > 0 && b[0] == messageToReplace {
				fmt.Println("replacing message to the server", messageType(b[0]).String())
				b = data
				encLevel = maxEncLevel(server, messageToReplaceEncLevel)
			}
			server.HandleMessage(b, encLevel)
		case c := <-sChunkChan:
			b := c.data
			encLevel := c.encLevel
			if len(b) > 0 && b[0] == messageToReplace {
				fmt.Println("replacing message to the client", messageType(b[0]).String())
				b = data
				encLevel = maxEncLevel(client, messageToReplaceEncLevel)
			}
			client.HandleMessage(b, encLevel)
		case <-done: // test done
			break messageLoop
		}
		if runner.errored {
			break messageLoop
		}
	}

	<-done
	if runner.errored {
		return 1
	}
	if sendPostHandshakeMessageToClient {
		if _, err := server.GetSessionTicket(); err != nil {
			panic(err)
		}
		client.HandleMessage(data, messageToReplaceEncLevel)
	}
	if sendPostHandshakeMessageToServer {
		server.HandleMessage(data, messageToReplaceEncLevel)
	}

	return 1
}
