package pileus

// gRPC-web bridge — no external dependencies.
//
// The browser cannot use native gRPC (HTTP/2 binary framing). This file
// implements the gRPC-web protocol: accept HTTP/1.1 from the browser, forward
// as HTTP/2 gRPC to the local gRPC server, then re-encode the response
// (appending trailers as a special frame) before sending back to the browser.
//
// gRPC-web framing is identical to gRPC for DATA frames:
//   [0x00][4-byte-BE-length][protobuf-bytes]
// Trailers are sent as an extra body frame with flag 0x80:
//   [0x80][4-byte-BE-length][key: value\r\n...]

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"strings"
	"sync"

	"golang.org/x/net/http2"
)

var (
	grpcH2Client   *http.Client
	grpcBridgeOnce sync.Once
)

// pinGRPCServerCert is the bridge's TLS VerifyConnection: it accepts the local
// gRPC server only if the leaf cert it presents matches tlsFingerprint (the
// SHA-256 Start() recorded for that same cert). Used instead of CA/hostname
// verification, which don't apply to a self-signed loopback listener.
func pinGRPCServerCert(cs tls.ConnectionState) error {
	if len(cs.PeerCertificates) == 0 {
		return errors.New("grpc-web bridge: gRPC server sent no certificate")
	}
	if tlsFingerprint == "" {
		return errors.New("grpc-web bridge: no pinned gRPC cert fingerprint (Start() not run?)")
	}
	sum := sha256.Sum256(cs.PeerCertificates[0].Raw)
	if got := hex.EncodeToString(sum[:]); got != tlsFingerprint {
		return fmt.Errorf("grpc-web bridge: gRPC server cert %s does not match pinned %s", got, tlsFingerprint)
	}
	return nil
}

func initGRPCWebBridge() {
	grpcBridgeOnce.Do(func() {
		t := &http2.Transport{
			// AllowHTTP lets us speak h2c (cleartext HTTP/2) to the local gRPC
			// server when it runs without TLS.
			AllowHTTP: true,
			DialTLSContext: func(ctx context.Context, network, addr string, cfg *tls.Config) (net.Conn, error) {
				d := &net.Dialer{}
				if !grpcListenTLS {
					return d.DialContext(ctx, network, addr)
				}
				// The gRPC server on grpcListenAddr speaks TLS (default) with a
				// self-signed cert: no CA chain to verify, no hostname to match
				// on this loopback hop. Instead we pin the leaf cert — its
				// SHA-256 is known in-process (tlsFingerprint, set by Start()).
				// A local process that raced us for the port therefore can't
				// impersonate the gRPC server.
				tc := &tls.Config{
					InsecureSkipVerify: true, //nolint:gosec // leaf cert pinned in pinGRPCServerCert
					NextProtos:         []string{"h2"},
					VerifyConnection:   pinGRPCServerCert,
				}
				if cfg != nil && cfg.ServerName != "" {
					tc.ServerName = cfg.ServerName
				}
				return tls.Dial(network, addr, tc)
			},
		}
		grpcH2Client = &http.Client{Transport: t}
	})
}

// GRPCWebHandler returns an HTTP handler that proxies gRPC-web requests to the
// local gRPC server. It must be mounted after Start() has been called so that
// grpcListenAddr is set.
func GRPCWebHandler() http.Handler {
	initGRPCWebBridge()
	return http.HandlerFunc(serveGRPCWeb)
}

func serveGRPCWeb(w http.ResponseWriter, r *http.Request) {
	ct := r.Header.Get("Content-Type")

	// CORS preflight
	if r.Method == http.MethodOptions {
		setCORSHeaders(w)
		// Echo whatever custom headers the browser asked for (the response is
		// Allow-Origin:* with no credentials, so this is safe) — covers
		// x-http-host / x-http-scheme and any future grpc-web metadata header
		// without having to enumerate them here.
		if reqH := r.Header.Get("Access-Control-Request-Headers"); reqH != "" {
			w.Header().Set("Access-Control-Allow-Headers", reqH)
		}
		w.WriteHeader(http.StatusNoContent)
		return
	}

	if !strings.HasPrefix(ct, "application/grpc-web") {
		http.NotFound(w, r)
		return
	}

	if grpcListenAddr == "" {
		http.Error(w, "gRPC server not ready", http.StatusServiceUnavailable)
		return
	}

	// Forward to the local gRPC server as a native gRPC/HTTP2 request. The
	// scheme must match how the server listens (TLS by default); the
	// http2.Transport's DialTLSContext handles both via grpcListenTLS.
	scheme := "http://"
	if grpcListenTLS {
		scheme = "https://"
	}
	grpcURL := scheme + grpcListenAddr + r.URL.Path
	grpcReq, err := http.NewRequestWithContext(r.Context(), http.MethodPost, grpcURL, r.Body)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	grpcReq.Header.Set("Content-Type", "application/grpc+proto")
	grpcReq.Header.Set("TE", "trailers")
	for k, vs := range r.Header {
		lk := strings.ToLower(k)
		if lk == "authorization" || lk == "grpc-timeout" || strings.HasPrefix(lk, "x-") {
			grpcReq.Header[k] = vs
		}
	}

	// Tell the gRPC server which origin the browser actually used to reach us, so
	// the resolver builds proxy URLs that are reachable from that same browser
	// (and on the right scheme — an https PWA can't load http media). An explicit
	// x-http-* from the client wins; otherwise derive it from this request's
	// forwarded/host headers. media_handler.go reads these from metadata.
	if grpcReq.Header.Get("X-Http-Host") == "" {
		if h := r.Header.Get("X-Forwarded-Host"); h != "" {
			grpcReq.Header.Set("X-Http-Host", h)
		} else if r.Host != "" {
			grpcReq.Header.Set("X-Http-Host", r.Host)
		}
	}
	if grpcReq.Header.Get("X-Http-Scheme") == "" {
		switch {
		case r.Header.Get("X-Forwarded-Proto") != "":
			grpcReq.Header.Set("X-Http-Scheme", r.Header.Get("X-Forwarded-Proto"))
		case r.TLS != nil:
			grpcReq.Header.Set("X-Http-Scheme", "https")
		default:
			grpcReq.Header.Set("X-Http-Scheme", "http")
		}
	}
	// Carry the browser's source IP through to the gRPC server so per-IP rate
	// limiting (e.g. on AuthorizeDevice) doesn't lump every web client into the
	// one loopback bucket. Set it to the socket peer we actually observed and
	// overwrite any client-supplied value — an inbound X-Forwarded-For here is
	// unauthenticated (no trusted-proxy list yet), so trusting it would let a
	// browser forge its own rate-limit key. Direct-to-mycelium (the common
	// case) this is the real browser IP; behind a same-host reverse proxy it's
	// the proxy and every web client shares one bucket (still enough to stop a
	// code-burning flood).
	if host, _, err := net.SplitHostPort(r.RemoteAddr); err == nil && host != "" {
		grpcReq.Header.Set("X-Forwarded-For", host)
	} else {
		grpcReq.Header.Del("X-Forwarded-For")
	}

	resp, err := grpcH2Client.Do(grpcReq)
	if err != nil {
		log.Printf("[grpcweb] %s → upstream dial/RPC error (target %s): %v", r.URL.Path, grpcURL, err)
		http.Error(w, "grpc-web bridge: "+err.Error(), http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()
	if gs := resp.Header.Get("Grpc-Status"); gs != "" && gs != "0" {
		log.Printf("[grpcweb] %s → grpc-status %s %q", r.URL.Path, gs, resp.Header.Get("Grpc-Message"))
	}

	setCORSHeaders(w)
	w.Header().Set("Content-Type", "application/grpc-web+proto")
	// gRPC-web requires 200 even when the RPC itself fails; status is in trailers.
	w.WriteHeader(http.StatusOK)

	flusher, canFlush := w.(http.Flusher)

	// Stream data frames straight through, flushing after each read so a
	// server-streaming RPC (e.g. ResolveStream's {progress} events) reaches the
	// browser as frames arrive, not all at once when the RPC ends. gRPC and
	// gRPC-web share the same 5-byte frame format, so no re-framing is needed.
	buf := make([]byte, 16<<10)
	for {
		n, rerr := resp.Body.Read(buf)
		if n > 0 {
			if _, werr := w.Write(buf[:n]); werr != nil {
				if r.Context().Err() == nil {
					log.Printf("[grpcweb] write body: %v", werr)
				}
				return
			}
			if canFlush {
				flusher.Flush()
			}
		}
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			if r.Context().Err() == nil {
				log.Printf("[grpcweb] read body: %v", rerr)
			}
			break
		}
	}

	// After the body is fully read, resp.Trailer is populated by the HTTP/2 client.
	if len(resp.Trailer) > 0 {
		var sb strings.Builder
		for k, vs := range resp.Trailer {
			for _, v := range vs {
				sb.WriteString(strings.ToLower(k))
				sb.WriteString(": ")
				sb.WriteString(v)
				sb.WriteString("\r\n")
			}
		}
		payload := []byte(sb.String())
		frame := make([]byte, 5+len(payload))
		frame[0] = 0x80 // trailer flag
		binary.BigEndian.PutUint32(frame[1:5], uint32(len(payload)))
		copy(frame[5:], payload)
		w.Write(frame) //nolint:errcheck
	}

	if canFlush {
		flusher.Flush()
	}
}

func setCORSHeaders(w http.ResponseWriter) {
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Access-Control-Allow-Methods", "POST, OPTIONS")
	w.Header().Set("Access-Control-Allow-Headers", "content-type, authorization, x-grpc-web, x-user-agent, x-profile-id, x-http-host, x-http-scheme, grpc-timeout")
	w.Header().Set("Access-Control-Expose-Headers", "grpc-status, grpc-message, grpc-status-details-bin")
}
