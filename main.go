package main

import (
	"bufio"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"flag"
	"fmt"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/eclipse/paho.golang/autopaho"
	"github.com/eclipse/paho.golang/autopaho/queue/file"
	"github.com/eclipse/paho.golang/paho"
	"github.com/justinas/alice"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/rs/zerolog"
	"github.com/rs/zerolog/hlog"
)

type purgeMessage struct {
	URL    *url.URL
	Header http.Header
	Sender string
}

func messagePublisher(ctx context.Context, wg *sync.WaitGroup, payloadChan chan []byte, logger zerolog.Logger, cm *autopaho.ConnectionManager, pubTopic string) {
	wg.Add(1)
	defer wg.Done()

	for payload := range payloadChan {
		if err := cm.PublishViaQueue(ctx, &autopaho.QueuePublish{
			Publish: &paho.Publish{
				QoS:     1,
				Topic:   pubTopic,
				Payload: payload,
			}}); err != nil {
			logger.Error().Err(err).Msg("failed queueing message")
		}

		logger.Info().Str("topic", pubTopic).Msg("added message to queue")

		// The queue relies on the file ModTime to work out what file is oldest; this means the resolution of
		// update times becomes important. To ensure order is maintained add a delay.
		time.Sleep(time.Millisecond)
	}

	logger.Info().Msg("messagePublisher: exiting")
}

func messageSubscriber(ctx context.Context, wg *sync.WaitGroup, msgChan chan *paho.Publish, logger zerolog.Logger, sender string, debug bool, handleLocalMessages bool) {
	wg.Add(1)
	defer wg.Done()

	var purgerWg sync.WaitGroup

	purgeClient := http.Client{}

	for msg := range msgChan {
		logger.Info().Str("topic", msg.Topic).Msg("got message from queue")

		pm := purgeMessage{}

		err := json.Unmarshal(msg.Payload, &pm)
		if err != nil {
			logger.Error().Err(err).Msg("unable to parse message")
			continue
		}

		if debug {
			marshalledJson, err := json.MarshalIndent(pm, "", "  ")
			if err != nil {
				logger.Error().Err(err).Msg("unable to parse message JSON")
			}
			fmt.Println("parsed JSON data:")
			fmt.Println(string(marshalledJson))
		}

		// We subscribe with the NoLocal flag so we should not be
		// seeing messages from ourselves, but check the sender field
		// just in case. This can happen e.g. if a previous instance
		// ran with "-danger-handle-local-messages" (meaning NoLocal
		// was false) and the session has not expired yet in the MQTT
		// server (in this case us setting NoLocal to true will have no
		// effect). If this happens you can exit the process and wait
		// for the session to expire before starting again.
		//
		// Since it is helpful to parse our own messages when testing
		// stuff we can skip this check via flag.
		if !handleLocalMessages {
			if pm.Sender == sender {
				logger.Info().Str("msg_sender", pm.Sender).Msg("skipping message sent by myself")
				continue
			}
		}

		go sendLocalPurge(ctx, &purgerWg, logger, purgeClient, pm)
	}

	logger.Info().Msg("messageSubscriber: waiting for outstanding local purge requests")
	purgerWg.Wait()

	logger.Info().Msg("messageSubscriber: exiting")
}

// This is the function that will be called to purge our local cache based on
// the contents of a purge message from another cache node.
func sendLocalPurge(ctx context.Context, wg *sync.WaitGroup, logger zerolog.Logger, purgeClient http.Client, pm purgeMessage) {
	wg.Add(1)
	defer wg.Done()

	req, err := http.NewRequestWithContext(ctx, "PURGE", "http://127.0.0.1:6081", nil)
	if err != nil {
		logger.Error().Err(err).Msg("unable to create HTTP request")
		return
	}

	// Mimic settings from the initial request recieved from a client
	req.Header = pm.Header
	req.URL.Path = pm.URL.Path

	resp, err := purgeClient.Do(req)
	if err != nil {
		logger.Error().Err(err).Msg("unable to send purge request")
		return
	}

	if resp.StatusCode != http.StatusOK {
		logger.Error().Int("status_code", resp.StatusCode).Msg("failed sending local purge")
	}
}

func varnishlogReader(ctx context.Context, wg *sync.WaitGroup, msgChan chan []byte, logger zerolog.Logger, sender string, debug bool) {
	wg.Add(1)
	defer wg.Done()

	purgesSent := promauto.NewCounter(prometheus.CounterOpts{
		Name: "sunet_cdnp_purge_notifies_sent_total",
		Help: "The total number of purge notifies sent",
	})

	cmd := exec.CommandContext(ctx, "varnishlog", "-n", "/var/lib/varnish/varnishd", "-q", `ReqMethod eq "PURGE" and RespStatus == 200 and ReqURL`, "-i", "Begin,ReqHeader,ReqURL,ReqStart,End")
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		logger.Fatal().Err(err).Msg("unable to create stdout pipe")
	}

	// If we do not setup a custom cmd.Cancel the call to cmd.Wait() will throw
	// an "context canceled" error if ctx is cancelled.
	cmd.Cancel = func() error {
		err = cmd.Process.Signal(syscall.SIGTERM)
		if err != nil {
			logger.Error().Err(err).Msg("sending signal failed")
			return err
		}
		// Make cmd.Wait() not return an error
		return os.ErrProcessDone
	}

	scanner := bufio.NewScanner(stdout)

	err = cmd.Start()
	if err != nil {
		logger.Fatal().Err(err).Msg("unable to start process")
	}

	// *   << Request  >> 1574921
	// -   Begin          req 1574920 rxreq
	// -   ReqStart       192.168.15.85 27017 a1
	// -   ReqURL         /
	// -   ReqHeader      host: www.example.com
	// -   ReqHeader      user-agent: curl/8.4.0
	// -   ReqHeader      accept: */*
	// -   ReqHeader      X-Forwarded-For: 192.168.15.85
	// -   ReqHeader      Via: 1.1 cdn-test-cache-1 (Varnish/7.4)
	// -   ReqHeader      X-Forwarded-Proto: https
	// -   End

	fieldMap := map[string]int{
		"tag":   3,
		"value": 9,
	}

	// Variables that will need to be reset any time we read a new
	// varnishlog header, see RESET comment below.
	pm := purgeMessage{}
	reqURL := ""
	header := http.Header{}
	clientIP := netip.Addr{}

	seenHeader := false
	for scanner.Scan() {
		text := scanner.Text()
		if debug {
			fmt.Printf("varnishlog line: %s\n", text)
		}
		if strings.HasPrefix(text, "*") {
			if !seenHeader {
				if debug {
					logger.Debug().Msg("found initial header")
				}
				seenHeader = true
			} else {
				if debug {
					logger.Debug().Msg("found next varnishlog header, resetting variables")
				}
				// RESET: Reset variables for new request we
				// are about to parse
				pm = purgeMessage{}
				reqURL = ""
				header = http.Header{}
				clientIP = netip.Addr{}
			}
		} else if strings.HasPrefix(text, "-") {
			if !seenHeader {
				logger.Fatal().Msg("got log message without seeing header first, this is odd")
			} else {
				// Maybe regex is more suitable to parse this
				// but keep to easy space-splitting for now.
				// There is also strings.Fields() but I am
				// worried we will lose information regarding
				// the exakt space contents inside header
				// values if we use that.
				// example result: []string{"-", "", "", "ReqHeader", "", "", "", "", "", "user-agent: curl/8.4.0"}
				fields := strings.SplitN(text, " ", 10)

				// Because varnishlog adds spaces to make
				// pretty columns it is possible there is
				// leftover leading space in the value. Clean
				// that up.
				trimmedValue := strings.TrimLeftFunc(fields[fieldMap["value"]], func(r rune) bool {
					// We could use unicode.IsSpace() here,
					// but be strict about the specific
					// whitespace characters we remove for
					// now.
					return r == ' '
				})
				if debug {
					logger.Debug().Str("varnishlog_tag", fields[fieldMap["tag"]]).Str("varnishlog_value", trimmedValue).Msg("varnishlog fields")
				}

				switch fields[fieldMap["tag"]] {
				case "ReqStart":
					if debug {
						logger.Debug().Str("trimmed_value", trimmedValue).Msg("got ReqStart")
					}
					reqStartFields := strings.Fields(trimmedValue)

					clientIP, err = netip.ParseAddr(reqStartFields[0])
					if err != nil {
						logger.Error().Err(err).Msg("unable to parse ReqStart IP address")
					}
				case "ReqURL":
					if debug {
						logger.Debug().Str("url", trimmedValue).Msg("got URL: %s\n")
					}
					reqURL = trimmedValue
				case "ReqHeader":
					headerParts := strings.SplitN(trimmedValue, ": ", 2)
					if len(headerParts) != 2 {
						logger.Error().Msg("unable to split header string")
						continue
					}
					headerKey := headerParts[0]
					headerValue := headerParts[1]

					// Filter on what header fields we want to
					// carry with us in the message.
					switch headerKey {
					case "xkey":
						header.Add(headerKey, headerValue)
					case "host":
						header.Add(headerKey, headerValue)
					case "X-Forwarded-Proto":
						header.Add(headerKey, headerValue)
					}
				case "End":
					// The entry is complete, finish
					// filling in struct and send it to
					// MQTT.
					if clientIP.IsLoopback() {
						// In order to not create loops
						// we ignore PURGE requests
						// that come from our own
						// machine as this is could be
						// a purge request sent by this
						// process in response to a
						// MQTT message from someone else.
						//
						// This could also be a result
						// of running a local curl or
						// similar, and we probably
						// should not spread those
						// either.
						logger.Info().Str("client_ip", clientIP.String()).Msg("not sending MQTT message for purge request from localhost")
						continue
					}

					pm.Sender = sender
					pm.Header = header

					urlString := ""

					if protos := header.Values("X-Forwarded-Proto"); protos != nil {
						if len(protos) != 1 {
							logger.Error().Int("num_protos", len(protos)).Msg("unexpected number of X-Forwarded-Proto header fields found")
						} else {
							urlString += protos[0] + "://"
						}
					}

					if hosts := header.Values("host"); hosts != nil {
						if len(hosts) != 1 {
							logger.Error().Int("num_hosts", len(hosts)).Msg("unexpected number of host header fields found")
						} else {
							urlString += hosts[0]
						}
					}

					urlString += reqURL

					u, err := url.Parse(urlString)
					if err != nil {
						logger.Error().Err(err).Msg("unable to parse URL")
					} else {
						pm.URL = u
					}

					var b []byte

					if debug {
						b, err = json.MarshalIndent(pm, "", "  ")
					} else {
						b, err = json.Marshal(pm)
					}
					if err != nil {
						logger.Error().Err(err).Msg("unable to marshal purgeMessage")
					} else {
						if debug {
							fmt.Println("about to send the following data:")
							fmt.Println(string(b))
						}
						msgChan <- b
						purgesSent.Inc()
					}

				}
			}
		} else if text == "" {
			// Do nothing, each log entry is trailed by an empty line
		} else {
			logger.Fatal().Str("varnishlog_line", text).Msg("found unexpected varnishlog line, exiting")
		}
	}

	if err := scanner.Err(); err != nil {
		logger.Fatal().Err(err).Msg("scanner failed")
	}

	err = cmd.Wait()
	if err != nil {
		logger.Fatal().Err(err).Msg("waiting on process failed")
	}

	logger.Info().Msg("varnishlogReader: exiting")
}

func setupMQTT(ctx context.Context, debug bool, logger *zerolog.Logger, logDir string, serverURL *url.URL, tlsConfig *tls.Config, subTopic string, handleLocalMessages bool) (*autopaho.ConnectionManager, chan *paho.Publish, error) {
	q, err := file.New(logDir, "queue", ".msg")
	if err != nil {
		return nil, nil, fmt.Errorf("setupMQTTPub(): unable to create file queue: %w", err)
	}

	subChan := make(chan *paho.Publish)

	errorLogger := logger.With().Str("paho_logger", "errors").Logger()
	pahoErrorLogger := logger.With().Str("paho_logger", "paho_errors").Logger()

	cliCfg := autopaho.ClientConfig{
		Queue:                         q,
		ServerUrls:                    []*url.URL{serverURL},
		TlsCfg:                        tlsConfig,
		KeepAlive:                     20,        // Keepalive message should be sent every 20 seconds
		CleanStartOnInitialConnection: false,     // Keep old messages in the broker in case we are missing
		SessionExpiryInterval:         86400 * 7, // If connection drops we want to keep messages in the broker for 7 days
		OnConnectionUp: func(cm *autopaho.ConnectionManager, connAck *paho.Connack) {
			logger.Info().Msg("pubsub: mqtt connection up")
			if _, err := cm.Subscribe(ctx, &paho.Subscribe{
				Subscriptions: []paho.SubscribeOptions{
					{
						Topic:   subTopic,
						QoS:     1,
						NoLocal: !handleLocalMessages, // No reason to recieve a message we sent ourselves other than for testing purposes
					},
				},
			}); err != nil {
				logger.Error().Err(err).Msg("subscribe: failed to subscribe. Probably due to connection drop so will retry")
				return // likely connection has dropped
			}
			logger.Info().Msg("subscribe: mqtt subscription made")
		},
		OnConnectError: func(err error) { logger.Error().Err(err).Msg("pubsub: error whilst attempting connection") },
		Errors:         &errorLogger,
		PahoErrors:     &pahoErrorLogger,
		// eclipse/paho.golang/paho provides base mqtt functionality, the below config will be passed in for each connection
		ClientConfig: paho.ClientConfig{
			ClientID: "sunet-cdnp-pubsub",
			OnPublishReceived: []func(paho.PublishReceived) (bool, error){
				func(pr paho.PublishReceived) (bool, error) {
					subChan <- pr.Packet
					return true, nil
				}},
			OnClientError: func(err error) { logger.Error().Err(err).Msg("pubsub: client error") },
			OnServerDisconnect: func(d *paho.Disconnect) {
				if d.Properties != nil {
					logger.Info().Str("reason_string", d.Properties.ReasonString).Msg("pubsub: server requested disconnect")
				} else {
					logger.Info().Uint8("reason_code", uint8(d.ReasonCode)).Msg("pubsub server requested disconnect")
				}
			},
		},
	}

	// Do not keep session around for long if we are doing testing stuff
	if handleLocalMessages {
		cliCfg.SessionExpiryInterval = 60
	}

	if debug {
		debugLogger := logger.With().Str("paho_logger", "debug").Logger()
		pahoDebugLogger := logger.With().Str("paho_logger", "paho_debug").Logger()

		cliCfg.Debug = &debugLogger
		cliCfg.PahoDebug = &pahoDebugLogger
	}

	c, err := autopaho.NewConnection(ctx, cliCfg)
	if err != nil {
		return nil, nil, fmt.Errorf("setupMQTT: unable to create connection: %w", err)
	}

	return c, subChan, nil

}

func newLogChain(logger zerolog.Logger) alice.Chain {
	c := alice.New()

	c = c.Append(hlog.NewHandler(logger))

	// Install some provided extra handler to set some request's context fields.
	// Thanks to that handler, all our logs will come with some prepopulated fields.
	c = c.Append(hlog.AccessHandler(func(r *http.Request, status, size int, duration time.Duration) {
		hlog.FromRequest(r).Info().
			Str("method", r.Method).
			Stringer("url", r.URL).
			Int("status", status).
			Int("size", size).
			Dur("duration", duration).
			Msg("")
	}))
	c = c.Append(hlog.RemoteAddrHandler("ip"))
	c = c.Append(hlog.UserAgentHandler("user_agent"))
	c = c.Append(hlog.RefererHandler("referer"))
	c = c.Append(hlog.RequestIDHandler("req_id", "Request-Id"))

	return c
}

func certPoolFromFile(fileName string) (*x509.CertPool, error) {
	fileName = filepath.Clean(fileName)
	cert, err := os.ReadFile(fileName)
	if err != nil {
		return nil, fmt.Errorf("certPoolFromFile: unable to read file: %w", err)
	}
	certPool := x509.NewCertPool()
	ok := certPool.AppendCertsFromPEM([]byte(cert))
	if !ok {
		return nil, fmt.Errorf("certPoolFromFile: failed to append certs from pem: %w", err)
	}

	return certPool, nil
}

func main() {
	debug := flag.Bool("debug", false, "enable debug logging")
	mqttClientCertFile := flag.String("mqtt-client-cert-file", "", "MQTT client cert file")
	mqttClientKeyFile := flag.String("mqtt-client-key-file", "", "MQTT client key file")
	mqttCAFile := flag.String("mqtt-ca-file", "", "MQTT trusted CA file, leave empty for OS default")
	mqttQueueDir := flag.String("mqtt-queue-dir", "/var/cache/sunet-cdnp/mqtt", "MQTT message queue directory")
	mqttPubTopic := flag.String("mqtt-pub-topic", "test/topic", "the topic we publish PURGE messages to")
	mqttSubTopic := flag.String("mqtt-sub-topic", "test/topic", "the topic we subscribe to PURGE messages on")
	httpServerAddr := flag.String("http-server-addr", "127.0.0.1:2112", "Address to bind HTTP server to")
	handleLocalMessages := flag.Bool("danger-handle-local-messages", false, "Handle messages sent by ourselves, should only be enabled for testing")
	flag.Parse()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	logger := zerolog.New(os.Stdout).With().
		Timestamp().
		Str("service", "sunet-cdnp").
		Logger()

	sender, err := os.Hostname()
	if err != nil {
		logger.Fatal().Err(err).Msg("unable to get hostname")
	}

	logger = logger.With().Str("sender", sender).Logger()

	newLogChain := newLogChain(logger)

	serverString := "tls://localhost:8883"
	serverURL, err := url.Parse(serverString)
	if err != nil {
		logger.Fatal().Err(err).Msg("unable to parse server string")
	}

	// Leaving these nil will use the OS default CA certs
	var mqttCACertPool *x509.CertPool

	// Setup client cert/key for mTLS authentication
	mqttClientCert, err := tls.LoadX509KeyPair(*mqttClientCertFile, *mqttClientKeyFile)
	if err != nil {
		logger.Fatal().Err(err).Msg("unable to load x509 MQTT client cert")
	}

	mqttCACertPool, err = certPoolFromFile(*mqttCAFile)
	if err != nil {
		logger.Fatal().Err(err).Msg("failed to create CA cert pool")
	}

	tlsCfg := &tls.Config{
		RootCAs:      mqttCACertPool,
		Certificates: []tls.Certificate{mqttClientCert},
		MinVersion:   tls.VersionTLS13,
	}

	// Make sure the queue dir exists
	err = os.MkdirAll(*mqttQueueDir, 0750)
	if err != nil {
		logger.Fatal().Err(err).Msg("unable to create MQTT queue directory")
	}

	mqttCM, subMsgChan, err := setupMQTT(ctx, *debug, &logger, *mqttQueueDir, serverURL, tlsCfg, *mqttSubTopic, *handleLocalMessages)
	if err != nil {
		logger.Fatal().Err(err).Msg("unable to setup MQTT publisher")
	}

	pubPayloadChan := make(chan []byte)
	mh := newLogChain.Then(promhttp.Handler())

	srv := &http.Server{
		Addr:           *httpServerAddr,
		ReadTimeout:    10 * time.Second,
		WriteTimeout:   10 * time.Second,
		MaxHeaderBytes: 1 << 20,
	}

	idleConnsClosed := make(chan struct{})
	go func() {
		<-ctx.Done()

		logger.Info().Msg("received signal, shutting down metrics HTTP server")

		// We received an interrupt signal, shut down.
		if err := srv.Shutdown(context.Background()); err != nil {
			// Error from closing listeners, or context timeout:
			logger.Error().Err(err).Msg("HTTP server Shutdown() error")
		}
		close(idleConnsClosed)
	}()

	var wg sync.WaitGroup

	go messagePublisher(ctx, &wg, pubPayloadChan, logger, mqttCM, *mqttPubTopic)
	go messageSubscriber(ctx, &wg, subMsgChan, logger, sender, *debug, *handleLocalMessages)

	go varnishlogReader(ctx, &wg, pubPayloadChan, logger, sender, *debug)

	http.Handle("/metrics", mh)

	err = srv.ListenAndServe()
	if err != http.ErrServerClosed {
		logger.Fatal().Err(err).Msg("HTTP server failed unexpectedly")
	}

	// Wait for connections to be gracefully closed.
	logger.Info().Msg("waiting for HTTP connections to complete")
	<-idleConnsClosed

	// The MQTT connection is automatically disconnected when a
	// signal is recevied by our signal.NotifyContext().
	// Wait here in case things have not finished shutting down yet.
	logger.Info().Msg("waiting for MQTT connection handler to shutdown")
	<-mqttCM.Done()

	// The HTTP server and MQTT connection is down at this point so our
	// message handlers can stop working now:
	close(pubPayloadChan)
	close(subMsgChan)

	// Wait for the message handlers to exit.
	logger.Info().Msg("waiting for message handlers to exit")
	wg.Wait()
}
