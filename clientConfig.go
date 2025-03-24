package kratos

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"github.com/xmidt-org/wrp-go/v3"
	"io/ioutil"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/gorilla/websocket"
	"github.com/xmidt-org/sallust"
	"go.uber.org/zap"
)

const (
	// Time allowed to write a message to the peer.
	writeWait = time.Duration(10) * time.Second

	// Time allowed to read the next pong message from the peer.
	pongWait = 60 * time.Second
)

var (
	errNilHandlePingMiss = errors.New("HandlePingMiss should not be nil")
)

// ClientConfig is the configuration to provide when making a new client.
type ClientConfig struct {
	DeviceName           string
	FirmwareName         string
	ModelName            string
	Manufacturer         string
	DestinationURL       string
	OutboundQueue        QueueConfig
	WRPEncoderQueue      QueueConfig
	WRPDecoderQueue      QueueConfig
	HandlerRegistryQueue QueueConfig
	HandleMsgQueue       QueueConfig
	Handlers             []HandlerConfig
	HandlePingMiss       HandlePingMiss
	ClientLogger         *zap.Logger
	PingConfig           PingConfig
	UseSSL               bool
	CertificatesPath     string
	PetasosEnabled       bool
   	Token                string
}

// QueueConfig is used to configure all the queues used to make kratos asynchronous.
type QueueConfig struct {
	MaxWorkers int
	Size       int
}

type PingConfig struct {
	PingWait    time.Duration
	MaxPingMiss int
}

// NewClient is used to create a new kratos Client from a ClientConfig.
func NewClient(config ClientConfig) (Client, error) {
	if config.HandlePingMiss == nil {
		return nil, errNilHandlePingMiss
	}

	inHeader := &clientHeader{
		deviceName:   config.DeviceName,
		firmwareName: config.FirmwareName,
		modelName:    config.ModelName,
		manufacturer: config.Manufacturer,
                token:        config.Token,
	}

	newConnection, connectionURL, err := createConnection(inHeader, config)

	if err != nil {
		return nil, err
	}

	pinged := make(chan string)
	newConnection.SetPingHandler(func(appData string) error {
		pinged <- appData
		err := newConnection.WriteControl(websocket.PongMessage, []byte(appData), time.Now().Add(writeWait))
		return err
	})

	// at this point we know that the URL connection is legitimate, so we can do some string manipulation
	// with the knowledge that `:` will be found in the string twice
	connectionURL = strings.TrimPrefix(connectionURL[len("ws://"):], ":")

	var logger *zap.Logger
	if config.ClientLogger != nil {
		logger = config.ClientLogger
	} else {
		logger = sallust.Default()
	}
	if config.PingConfig.MaxPingMiss <= 0 {
		config.PingConfig.MaxPingMiss = 1
	}
	if config.PingConfig.PingWait == 0 {
		config.PingConfig.PingWait = time.Minute
	}

	sender := NewSender(newConnection, config.OutboundQueue.MaxWorkers, config.OutboundQueue.Size, logger)
	encoder := NewEncoderSender(sender, config.WRPEncoderQueue.MaxWorkers, config.WRPEncoderQueue.Size, logger)

	newClient := &client{
		deviceID:        inHeader.deviceName,
		userAgent:       "WebPA-1.6(" + inHeader.firmwareName + ";" + inHeader.modelName + "/" + inHeader.manufacturer + ";)",
		deviceProtocols: "TODO-what-to-put-here",
		hostname:        connectionURL,
		handlePingMiss:  config.HandlePingMiss,
		encoderSender:   encoder,
		connection:      newConnection,
		headerInfo:      inHeader,
		done:            make(chan struct{}, 1),
		logger:          logger,
		pingConfig:      config.PingConfig,
	}

	newClient.registry, err = NewHandlerRegistry(config.Handlers)
	if err != nil {
		logger.Warn("failed to initialize all handlers for registry", zap.Error(err))
	}

	downstreamSender := NewDownstreamSender(newClient.Send, config.HandleMsgQueue.MaxWorkers, config.HandleMsgQueue.Size, logger)
	registryHandler := NewRegistryHandler(newClient.Send, newClient.registry, downstreamSender, config.HandlerRegistryQueue.MaxWorkers, config.HandlerRegistryQueue.Size, newClient.deviceID, logger)
	decoder := NewDecoderSender(registryHandler, config.WRPDecoderQueue.MaxWorkers, config.WRPDecoderQueue.Size, logger)
	newClient.decoderSender = decoder

	pingTimer := time.NewTimer(newClient.pingConfig.PingWait)

	newClient.wg.Add(2)
	go newClient.checkPing(pingTimer, pinged)

	go newClient.read()

	return newClient, nil
}

// private func used to generate the client that we're looking to produce
func createConnection(headerInfo *clientHeader, config ClientConfig) (connection *websocket.Conn, wsURL string, err error) {
	_, err = wrp.ParseDeviceID(headerInfo.deviceName)

	if err != nil {
		return nil, "", err
	}
	var talariaInstance = ""
	tlsConfig := GetTLSConfig(strings.Split(config.DeviceName, ":")[1], config.CertificatesPath, config.UseSSL)
	if config.PetasosEnabled {
		talariaInstance, err = getTalariaInstance(config, tlsConfig)
		if err != nil {
			fmt.Println("Error while fetching Talaria for: ", config.DeviceName, err)
			return nil, "", err
		}
	} else {
		talariaInstance = config.DestinationURL
	}

	dialer := &websocket.Dialer{}
	if config.UseSSL {
		dialer.TLSClientConfig = tlsConfig
	}

	// make a header and put some data in that (including MAC address)
	// TODO: find special function for user agent
	headers := make(http.Header)
	headers.Add("X-Webpa-Device-Name", headerInfo.deviceName)
	headers.Add("X-Webpa-Firmware-Name", headerInfo.firmwareName)
	headers.Add("X-Webpa-Model-Name", headerInfo.modelName)
	headers.Add("X-Webpa-Manufacturer", headerInfo.manufacturer)
	headers.Add("Authorization", "Bearer "+headerInfo.token)

	// make sure destUrl's protocol is websocket (ws)
	wsURL = strings.Replace(talariaInstance, "http", "ws", 1)

	// creates a new client connection given the URL string
	connection, resp, err := dialer.Dial(wsURL, headers)

	for err == websocket.ErrBadHandshake && resp != nil && resp.StatusCode == http.StatusTemporaryRedirect {
		fmt.Println(err)
		// Get url to which we are redirected and reconfigure it
		wsURL = strings.Replace(resp.Header.Get("Location"), "http", "ws", 1)

		connection, resp, err = dialer.Dial(wsURL, headers)
	}

	if err != nil {
		if resp != nil {
			err = createHTTPError(resp, err)
		}
		return nil, "", err
	}

	return connection, wsURL, nil
}

func getTalariaInstance(config ClientConfig, tlsConfig *tls.Config) (string, error) {

	// Create HTTP client with custom transport

	tr := &http.Transport{
		TLSClientConfig: func() *tls.Config {
			if config.UseSSL {
				return tlsConfig
			} else {
				return nil
			}
		}(),
		DisableCompression: true,
		DisableKeepAlives:  true,
	}
	client := &http.Client{Transport: tr, CheckRedirect: func(req *http.Request, via []*http.Request) error {
		return http.ErrUseLastResponse
	}}

	// Create HTTP request with headers
	req, err := http.NewRequest("GET", config.DestinationURL, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("X-Webpa-Device-Name", config.DeviceName)

	// Send HTTP request and get response
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusTemporaryRedirect {
		return "", errors.New("petasos didn't respond")
	}

	if resp.Header.Get("Location") != "" {
   		 location := strings.TrimPrefix(resp.Header.Get("Location"), "https,")
   		 return location, nil
	}

	// Read response body
	body, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}
	pattern := `((?:https?://|www\d{0,3}[.]|[a-z0-9.\-]+[.][a-z]{2,4}/)(?:[^\s()<>]+|\(([^\s()<>]+|(\([^\s()<>]+\)))*\))+(?:\(([^\s()<>]+|(\([^\s()<>]+\)))*\)|[^\s!()\[\]{};:\'".,<>?«»“”‘’]))`

	re := regexp.MustCompile(pattern)
	match := re.FindString(string(body))
	return match, nil
}

func GetTLSConfig(macAddress string, certificatesPath string, useSSL bool) *tls.Config {
	if !useSSL {
		return nil
	}
	certFile := fmt.Sprintf("%s/%s-client.crt", certificatesPath, macAddress)
	keyFile := fmt.Sprintf("%s/%s-key.pem", certificatesPath, macAddress)
	caFile := fmt.Sprintf("%s/ca.crt", certificatesPath)

	// Try reading the certificate files with the prefix of the provided macaddress
	cert, err := tls.LoadX509KeyPair(certFile, keyFile)
	if err != nil {
		// If that fails, try reading the certificate files without the prefix
		certFile = fmt.Sprintf("%s/client.crt", certificatesPath)
		keyFile = fmt.Sprintf("%s/key.pem", certificatesPath)
		cert, err = tls.LoadX509KeyPair(certFile, keyFile)
		if err != nil {
			fmt.Println(err)
			return nil
		}
	}

	caCert, err := ioutil.ReadFile(caFile)
	if err != nil {
		return nil
	}
	caCertPool := x509.NewCertPool()
	caCertPool.AppendCertsFromPEM(caCert)

	return &tls.Config{
		RootCAs:      caCertPool,
		Certificates: []tls.Certificate{cert},
	}

}
