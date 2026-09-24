package transport

import (
	"context"
	"crypto/tls"
	"net"
	"testing"
	"time"
)

func BenchmarkTransportSecurity(b *testing.B) {
	ca := NewTestCA(b, "Lattice Benchmark CA")
	serverCertFile, serverKeyFile := ca.IssueServerCert(b, "localhost")
	clientCertFile, clientKeyFile := ca.IssueClientCert(b, "client")

	// Sub-benchmark 1: Plaintext TCP Baseline
	b.Run("Plaintext_TCP_Baseline", func(b *testing.B) {
		handler := newMockEngine()
		_ = handler.Put(context.Background(), []byte("key"), []byte("benchmark-value"))

		srv, err := NewServer(ServerConfig{
			HeaderTimeout:     5 * time.Second,
			PayloadTimeout:    5 * time.Second,
			IdleTimeout:       60 * time.Second,
			WriteTimeout:      5 * time.Second,
			InsecureTransport: true,
		}, handler)
		if err != nil {
			b.Fatalf("NewServer failed: %v", err)
		}

		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			b.Fatalf("net.Listen failed: %v", err)
		}
		defer ln.Close()

		go func() { _ = srv.Serve(ln) }()
		defer func() {
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			_ = srv.Shutdown(ctx)
		}()

		conn, err := net.Dial("tcp", ln.Addr().String())
		if err != nil {
			b.Fatalf("dial failed: %v", err)
		}
		defer conn.Close()

		req := &Request{OpCode: OpGet, SeqID: 1, Key: []byte("key")}
		b.ResetTimer()
		b.ReportAllocs()

		for i := 0; i < b.N; i++ {
			req.SeqID = uint64(i + 1)
			if err := WriteRequest(conn, req); err != nil {
				b.Fatalf("write failed: %v", err)
			}
			resp, err := ReadResponse(conn)
			if err != nil {
				b.Fatalf("read failed: %v", err)
			}
			if resp.Status != StatusOk {
				b.Fatalf("unexpected status: %v", resp.Status)
			}
		}
	})

	// Sub-benchmark 2: TLS Server Authentication
	b.Run("TLS_Server_Auth", func(b *testing.B) {
		handler := newMockEngine()
		_ = handler.Put(context.Background(), []byte("key"), []byte("benchmark-value"))

		srv, err := NewServer(ServerConfig{
			HeaderTimeout:  5 * time.Second,
			PayloadTimeout: 5 * time.Second,
			IdleTimeout:    60 * time.Second,
			WriteTimeout:   5 * time.Second,
			TLSCertFile:    serverCertFile,
			TLSKeyFile:     serverKeyFile,
		}, handler)
		if err != nil {
			b.Fatalf("NewServer failed: %v", err)
		}

		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			b.Fatalf("net.Listen failed: %v", err)
		}
		defer ln.Close()

		go func() { _ = srv.Serve(ln) }()
		defer func() {
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			_ = srv.Shutdown(ctx)
		}()

		clientTLS, err := ClientTLSConfig(ca.CertPath, "", "", "localhost", false)
		if err != nil {
			b.Fatalf("ClientTLSConfig failed: %v", err)
		}
		conn, err := tls.Dial("tcp", ln.Addr().String(), clientTLS)
		if err != nil {
			b.Fatalf("tls.Dial failed: %v", err)
		}
		defer conn.Close()

		req := &Request{OpCode: OpGet, SeqID: 1, Key: []byte("key")}
		b.ResetTimer()
		b.ReportAllocs()

		for i := 0; i < b.N; i++ {
			req.SeqID = uint64(i + 1)
			if err := WriteRequest(conn, req); err != nil {
				b.Fatalf("write failed: %v", err)
			}
			resp, err := ReadResponse(conn)
			if err != nil {
				b.Fatalf("read failed: %v", err)
			}
			if resp.Status != StatusOk {
				b.Fatalf("unexpected status: %v", resp.Status)
			}
		}
	})

	// Sub-benchmark 3: Mutual TLS (mTLS) Client Authentication
	b.Run("mTLS_Client_Auth", func(b *testing.B) {
		handler := newMockEngine()
		_ = handler.Put(context.Background(), []byte("key"), []byte("benchmark-value"))

		srv, err := NewServer(ServerConfig{
			HeaderTimeout:     5 * time.Second,
			PayloadTimeout:    5 * time.Second,
			IdleTimeout:       60 * time.Second,
			WriteTimeout:      5 * time.Second,
			TLSCertFile:       serverCertFile,
			TLSKeyFile:        serverKeyFile,
			ClientCAFile:      ca.CertPath,
			RequireClientCert: true,
		}, handler)
		if err != nil {
			b.Fatalf("NewServer failed: %v", err)
		}

		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			b.Fatalf("net.Listen failed: %v", err)
		}
		defer ln.Close()

		go func() { _ = srv.Serve(ln) }()
		defer func() {
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			_ = srv.Shutdown(ctx)
		}()

		clientTLS, err := ClientTLSConfig(ca.CertPath, clientCertFile, clientKeyFile, "localhost", false)
		if err != nil {
			b.Fatalf("ClientTLSConfig failed: %v", err)
		}
		conn, err := tls.Dial("tcp", ln.Addr().String(), clientTLS)
		if err != nil {
			b.Fatalf("tls.Dial failed: %v", err)
		}
		defer conn.Close()

		req := &Request{OpCode: OpGet, SeqID: 1, Key: []byte("key")}
		b.ResetTimer()
		b.ReportAllocs()

		for i := 0; i < b.N; i++ {
			req.SeqID = uint64(i + 1)
			if err := WriteRequest(conn, req); err != nil {
				b.Fatalf("write failed: %v", err)
			}
			resp, err := ReadResponse(conn)
			if err != nil {
				b.Fatalf("read failed: %v", err)
			}
			if resp.Status != StatusOk {
				b.Fatalf("unexpected status: %v", resp.Status)
			}
		}
	})
}
