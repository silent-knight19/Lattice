package transport

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"net"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/silent-knight19/lattice/internal/binary"
	"github.com/silent-knight19/lattice/internal/errors"
)

// countingMockEngine instruments storage operations to verify that denied
// requests are rejected BEFORE storage side effects occur.
type countingMockEngine struct {
	mu          sync.RWMutex
	store       map[string][]byte
	putCount    atomic.Int64
	getCount    atomic.Int64
	deleteCount atomic.Int64
	existsCount atomic.Int64
	batchCount  atomic.Int64
	statsCount  atomic.Int64
}

func newCountingMockEngine() *countingMockEngine {
	return &countingMockEngine{
		store: make(map[string][]byte),
	}
}

func (m *countingMockEngine) Put(ctx context.Context, key, val []byte) error {
	m.putCount.Add(1)
	m.mu.Lock()
	defer m.mu.Unlock()
	cp := make([]byte, len(val))
	copy(cp, val)
	m.store[string(key)] = cp
	return nil
}

func (m *countingMockEngine) Get(key []byte) ([]byte, error) {
	m.getCount.Add(1)
	m.mu.RLock()
	defer m.mu.RUnlock()
	val, ok := m.store[string(key)]
	if !ok {
		return nil, errors.ErrKeyNotFound
	}
	cp := make([]byte, len(val))
	copy(cp, val)
	return cp, nil
}

func (m *countingMockEngine) Delete(ctx context.Context, key []byte) error {
	m.deleteCount.Add(1)
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.store, string(key))
	return nil
}

func (m *countingMockEngine) Exists(key []byte) (bool, error) {
	m.existsCount.Add(1)
	m.mu.RLock()
	defer m.mu.RUnlock()
	_, ok := m.store[string(key)]
	return ok, nil
}

func (m *countingMockEngine) Batch(ctx context.Context, batch []binary.BatchOp) error {
	m.batchCount.Add(1)
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, op := range batch {
		if op.Type == binary.OpTypePut {
			cp := make([]byte, len(op.Value))
			copy(cp, op.Value)
			m.store[string(op.Key)] = cp
		} else {
			delete(m.store, string(op.Key))
		}
	}
	return nil
}

func (m *countingMockEngine) Stats() (EngineStats, MemoryStats, StorageStats, CacheStats, error) {
	m.statsCount.Add(1)
	return EngineStats{}, MemoryStats{}, StorageStats{}, CacheStats{}, nil
}

// countingMockRouter implements both ProposalRouter and ReadRouter with call tracking.
type countingMockRouter struct {
	writeCount atomic.Int64
	readCount  atomic.Int64
}

func (r *countingMockRouter) RouteWrite(ctx context.Context, req *Request) (*Response, error) {
	r.writeCount.Add(1)
	return &Response{
		OpCode: req.OpCode,
		SeqID:  req.SeqID,
		Status: StatusOk,
	}, nil
}

func (r *countingMockRouter) RouteRead(ctx context.Context, req *Request) (*Response, error) {
	r.readCount.Add(1)
	return &Response{
		OpCode: req.OpCode,
		SeqID:  req.SeqID,
		Status: StatusOk,
		Value:  []byte("routed_val"),
	}, nil
}

// certFingerprintFromFile extracts and computes SHA-256 fingerprint from a PEM certificate file.
func certFingerprintFromFile(t testing.TB, certPath string) string {
	t.Helper()
	data, err := os.ReadFile(certPath)
	if err != nil {
		t.Fatalf("failed to read cert file %s: %v", certPath, err)
	}
	block, _ := pem.Decode(data)
	if block == nil {
		t.Fatalf("failed to decode PEM block from %s", certPath)
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatalf("failed to parse cert PEM %s: %v", certPath, err)
	}
	return CertificateFingerprintSHA256(cert)
}

// setupAuthzTestServer boots a TLS server with a configured authorization policy and returns client dial config.
func setupAuthzTestServer(t testing.TB, ca *TestCA, policy map[string]string, eng Engine, router *countingMockRouter, clusterMode bool) (addr string, cleanup func()) {
	t.Helper()

	srvCert, srvKey := ca.IssueServerCert(t, "server")

	cfg := DefaultServerConfig()
	cfg.Address = "127.0.0.1:0"
	cfg.TLSCertFile = srvCert
	cfg.TLSKeyFile = srvKey
	cfg.ClientCAFile = ca.CertPath
	cfg.RequireClientCert = true
	cfg.ClientAuthzPolicy = policy
	cfg.ClusterMode = clusterMode
	if router != nil {
		cfg.ProposalRouter = router
		cfg.ReadRouter = router
	}

	srv, err := NewServer(cfg, eng)
	if err != nil {
		t.Fatalf("NewServer failed: %v", err)
	}

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Listen failed: %v", err)
	}

	go func() { _ = srv.Serve(ln) }()

	cleanup = func() {
		_ = srv.Shutdown(context.Background())
		_ = ln.Close()
	}

	return ln.Addr().String(), cleanup
}

func dialClientWithCert(t testing.TB, addr string, ca *TestCA, certPath, keyPath string) *tls.Conn {
	t.Helper()
	clientCfg, err := ClientTLSConfig(ca.CertPath, certPath, keyPath, "localhost", false)
	if err != nil {
		t.Fatalf("ClientTLSConfig failed: %v", err)
	}
	conn, err := tls.Dial("tcp", addr, clientCfg)
	if err != nil {
		t.Fatalf("tls.Dial to %s failed: %v", addr, err)
	}
	return conn
}

// -----------------------------------------------------------------------------
// Authentication-to-authorization binding tests (1 - 11)
// -----------------------------------------------------------------------------

func TestAuthz_Reader_Can_Get(t *testing.T) {
	eng := newCountingMockEngine()
	_ = eng.Put(context.Background(), []byte("key1"), []byte("val1"))

	ca := NewTestCA(t, "ca")
	readerCert, readerKey := ca.IssueClientCert(t, "reader-client")
	fp := certFingerprintFromFile(t, readerCert)

	policy := map[string]string{fp: "reader"}
	addr, cleanup := setupAuthzTestServer(t, ca, policy, eng, nil, false)
	defer cleanup()

	conn := dialClientWithCert(t, addr, ca, readerCert, readerKey)
	defer conn.Close()

	req := &Request{OpCode: OpGet, Key: []byte("key1"), SeqID: 1}
	if err := WriteRequest(conn, req); err != nil {
		t.Fatalf("WriteRequest failed: %v", err)
	}
	resp, err := ReadResponse(conn)
	if err != nil {
		t.Fatalf("ReadResponse failed: %v", err)
	}
	if resp.Status != StatusOk {
		t.Fatalf("expected StatusOk, got 0x%02x (%s)", resp.Status, resp.Message)
	}
	if string(resp.Value) != "val1" {
		t.Fatalf("expected val1, got %s", resp.Value)
	}
}

func TestAuthz_Reader_Can_Exists(t *testing.T) {
	eng := newCountingMockEngine()
	_ = eng.Put(context.Background(), []byte("key1"), []byte("val1"))

	ca := NewTestCA(t, "ca")
	readerCert, readerKey := ca.IssueClientCert(t, "reader-client")
	fp := certFingerprintFromFile(t, readerCert)

	policy := map[string]string{fp: "reader"}
	addr, cleanup := setupAuthzTestServer(t, ca, policy, eng, nil, false)
	defer cleanup()

	conn := dialClientWithCert(t, addr, ca, readerCert, readerKey)
	defer conn.Close()

	req := &Request{OpCode: OpExists, Key: []byte("key1"), SeqID: 1}
	if err := WriteRequest(conn, req); err != nil {
		t.Fatalf("WriteRequest failed: %v", err)
	}
	resp, err := ReadResponse(conn)
	if err != nil {
		t.Fatalf("ReadResponse failed: %v", err)
	}
	if resp.Status != StatusOk {
		t.Fatalf("expected StatusOk, got 0x%02x (%s)", resp.Status, resp.Message)
	}
	if !resp.Exists {
		t.Fatalf("expected Exists=true, got false")
	}
}

func TestAuthz_Reader_Can_Stats(t *testing.T) {
	eng := newCountingMockEngine()

	ca := NewTestCA(t, "ca")
	readerCert, readerKey := ca.IssueClientCert(t, "reader-client")
	fp := certFingerprintFromFile(t, readerCert)

	policy := map[string]string{fp: "reader"}
	addr, cleanup := setupAuthzTestServer(t, ca, policy, eng, nil, false)
	defer cleanup()

	conn := dialClientWithCert(t, addr, ca, readerCert, readerKey)
	defer conn.Close()

	req := &Request{OpCode: OpStats, SeqID: 1}
	if err := WriteRequest(conn, req); err != nil {
		t.Fatalf("WriteRequest failed: %v", err)
	}
	resp, err := ReadResponse(conn)
	if err != nil {
		t.Fatalf("ReadResponse failed: %v", err)
	}
	if resp.Status != StatusOk {
		t.Fatalf("expected StatusOk, got 0x%02x (%s)", resp.Status, resp.Message)
	}
	if len(resp.Value) == 0 {
		t.Fatalf("expected non-empty stats payload")
	}
}

func TestAuthz_Reader_Denied_Put(t *testing.T) {
	eng := newCountingMockEngine()

	ca := NewTestCA(t, "ca")
	readerCert, readerKey := ca.IssueClientCert(t, "reader-client")
	fp := certFingerprintFromFile(t, readerCert)

	policy := map[string]string{fp: "reader"}
	addr, cleanup := setupAuthzTestServer(t, ca, policy, eng, nil, false)
	defer cleanup()

	conn := dialClientWithCert(t, addr, ca, readerCert, readerKey)
	defer conn.Close()

	req := &Request{OpCode: OpPut, Key: []byte("k"), Value: []byte("v"), SeqID: 42}
	if err := WriteRequest(conn, req); err != nil {
		t.Fatalf("WriteRequest failed: %v", err)
	}
	resp, err := ReadResponse(conn)
	if err != nil {
		t.Fatalf("ReadResponse failed: %v", err)
	}
	if resp.Status != StatusPermissionDenied {
		t.Fatalf("expected StatusPermissionDenied (0x07), got 0x%02x (%s)", resp.Status, resp.Message)
	}
	if resp.Message != "permission denied" {
		t.Fatalf("expected bounded message 'permission denied', got %q", resp.Message)
	}
	if eng.putCount.Load() != 0 {
		t.Fatalf("SECURITY VIOLATION: Engine.Put was invoked %d times for unauthorized reader", eng.putCount.Load())
	}
}

func TestAuthz_Reader_Denied_Delete(t *testing.T) {
	eng := newCountingMockEngine()

	ca := NewTestCA(t, "ca")
	readerCert, readerKey := ca.IssueClientCert(t, "reader-client")
	fp := certFingerprintFromFile(t, readerCert)

	policy := map[string]string{fp: "reader"}
	addr, cleanup := setupAuthzTestServer(t, ca, policy, eng, nil, false)
	defer cleanup()

	conn := dialClientWithCert(t, addr, ca, readerCert, readerKey)
	defer conn.Close()

	req := &Request{OpCode: OpDelete, Key: []byte("k"), SeqID: 43}
	if err := WriteRequest(conn, req); err != nil {
		t.Fatalf("WriteRequest failed: %v", err)
	}
	resp, err := ReadResponse(conn)
	if err != nil {
		t.Fatalf("ReadResponse failed: %v", err)
	}
	if resp.Status != StatusPermissionDenied {
		t.Fatalf("expected StatusPermissionDenied, got 0x%02x (%s)", resp.Status, resp.Message)
	}
	if eng.deleteCount.Load() != 0 {
		t.Fatalf("SECURITY VIOLATION: Engine.Delete was invoked for unauthorized reader")
	}
}

func TestAuthz_Reader_Denied_Batch(t *testing.T) {
	eng := newCountingMockEngine()

	ca := NewTestCA(t, "ca")
	readerCert, readerKey := ca.IssueClientCert(t, "reader-client")
	fp := certFingerprintFromFile(t, readerCert)

	policy := map[string]string{fp: "reader"}
	addr, cleanup := setupAuthzTestServer(t, ca, policy, eng, nil, false)
	defer cleanup()

	conn := dialClientWithCert(t, addr, ca, readerCert, readerKey)
	defer conn.Close()

	req := &Request{
		OpCode: OpBatch,
		SeqID:  44,
		Batch: []BatchOp{
			{Type: BatchOpPut, Key: []byte("k1"), Value: []byte("v1")},
		},
	}
	if err := WriteRequest(conn, req); err != nil {
		t.Fatalf("WriteRequest failed: %v", err)
	}
	resp, err := ReadResponse(conn)
	if err != nil {
		t.Fatalf("ReadResponse failed: %v", err)
	}
	if resp.Status != StatusPermissionDenied {
		t.Fatalf("expected StatusPermissionDenied, got 0x%02x (%s)", resp.Status, resp.Message)
	}
	if eng.batchCount.Load() != 0 {
		t.Fatalf("SECURITY VIOLATION: Engine.Batch was invoked for unauthorized reader")
	}
}

func TestAuthz_Writer_Can_All_Ops(t *testing.T) {
	eng := newCountingMockEngine()

	ca := NewTestCA(t, "ca")
	writerCert, writerKey := ca.IssueClientCert(t, "writer-client")
	fp := certFingerprintFromFile(t, writerCert)

	policy := map[string]string{fp: "writer"}
	addr, cleanup := setupAuthzTestServer(t, ca, policy, eng, nil, false)
	defer cleanup()

	conn := dialClientWithCert(t, addr, ca, writerCert, writerKey)
	defer conn.Close()

	// 1. PUT
	reqPut := &Request{OpCode: OpPut, Key: []byte("wkey"), Value: []byte("wval"), SeqID: 1}
	if err := WriteRequest(conn, reqPut); err != nil {
		t.Fatalf("PUT failed: %v", err)
	}
	respPut, _ := ReadResponse(conn)
	if respPut.Status != StatusOk {
		t.Fatalf("PUT: expected StatusOk, got 0x%02x", respPut.Status)
	}

	// 2. GET
	reqGet := &Request{OpCode: OpGet, Key: []byte("wkey"), SeqID: 2}
	_ = WriteRequest(conn, reqGet)
	respGet, _ := ReadResponse(conn)
	if respGet.Status != StatusOk || string(respGet.Value) != "wval" {
		t.Fatalf("GET: expected StatusOk with wval, got status 0x%02x val %s", respGet.Status, respGet.Value)
	}

	// 3. EXISTS
	reqExists := &Request{OpCode: OpExists, Key: []byte("wkey"), SeqID: 3}
	_ = WriteRequest(conn, reqExists)
	respExists, _ := ReadResponse(conn)
	if respExists.Status != StatusOk || !respExists.Exists {
		t.Fatalf("EXISTS: expected StatusOk and true, got status 0x%02x exists=%v", respExists.Status, respExists.Exists)
	}

	// 4. STATS
	reqStats := &Request{OpCode: OpStats, SeqID: 4}
	_ = WriteRequest(conn, reqStats)
	respStats, _ := ReadResponse(conn)
	if respStats.Status != StatusOk {
		t.Fatalf("STATS: expected StatusOk, got 0x%02x", respStats.Status)
	}

	// 5. BATCH
	reqBatch := &Request{
		OpCode: OpBatch,
		SeqID:  5,
		Batch: []BatchOp{
			{Type: BatchOpPut, Key: []byte("bkey"), Value: []byte("bval")},
		},
	}
	_ = WriteRequest(conn, reqBatch)
	respBatch, _ := ReadResponse(conn)
	if respBatch.Status != StatusOk {
		t.Fatalf("BATCH: expected StatusOk, got 0x%02x", respBatch.Status)
	}

	// 6. DELETE
	reqDel := &Request{OpCode: OpDelete, Key: []byte("wkey"), SeqID: 6}
	_ = WriteRequest(conn, reqDel)
	respDel, _ := ReadResponse(conn)
	if respDel.Status != StatusOk {
		t.Fatalf("DELETE: expected StatusOk, got 0x%02x", respDel.Status)
	}
}

func TestAuthz_Unknown_Valid_CA_Cert_Denied(t *testing.T) {
	eng := newCountingMockEngine()

	ca := NewTestCA(t, "ca")
	unknownCert, unknownKey := ca.IssueClientCert(t, "unknown-client")
	// Configure policy with a different dummy fingerprint
	dummyFP := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	policy := map[string]string{dummyFP: "reader"}

	addr, cleanup := setupAuthzTestServer(t, ca, policy, eng, nil, false)
	defer cleanup()

	conn := dialClientWithCert(t, addr, ca, unknownCert, unknownKey)
	defer conn.Close()

	req := &Request{OpCode: OpGet, Key: []byte("any"), SeqID: 1}
	_ = WriteRequest(conn, req)
	resp, err := ReadResponse(conn)
	if err != nil {
		t.Fatalf("ReadResponse failed: %v", err)
	}
	if resp.Status != StatusPermissionDenied {
		t.Fatalf("expected StatusPermissionDenied for unknown cert, got 0x%02x (%s)", resp.Status, resp.Message)
	}
}

func TestAuthz_Invalid_Fingerprint_Fails_Construction(t *testing.T) {
	eng := newCountingMockEngine()
	invalidFPs := []string{
		"",          // empty
		"too-short", // short
		"zzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzz",   // non-hex
		"1234567890abcdef1234567890abcdef1234567890abcdef1234567890abcdef12", // 66 chars
	}

	for _, badFP := range invalidFPs {
		cfg := DefaultServerConfig()
		cfg.ClientAuthzPolicy = map[string]string{badFP: "reader"}
		_, err := NewServer(cfg, eng)
		if err == nil {
			t.Fatalf("expected NewServer to fail for invalid fingerprint %q, got nil", badFP)
		}
	}
}

func TestAuthz_Unsupported_Role_Fails_Construction(t *testing.T) {
	eng := newCountingMockEngine()
	validFP := "1111111111111111111111111111111111111111111111111111111111111111"
	badRoles := []string{"", "superadmin", "operator", "root", "guest", "read_only"}

	for _, badRole := range badRoles {
		cfg := DefaultServerConfig()
		cfg.ClientAuthzPolicy = map[string]string{validFP: badRole}
		_, err := NewServer(cfg, eng)
		if err == nil {
			t.Fatalf("expected NewServer to fail for unsupported role %q, got nil", badRole)
		}
	}
}

func TestAuthz_Duplicate_Policy_Entries(t *testing.T) {
	validFP := "1111111111111111111111111111111111111111111111111111111111111111"

	// Conflicting duplicate entries must fail
	conflictingEntries := []PolicyEntry{
		{Fingerprint: validFP, Role: RoleReader},
		{Fingerprint: validFP, Role: RoleWriter},
	}
	_, err := NewAuthzPolicyFromEntries(conflictingEntries)
	if err == nil {
		t.Fatalf("expected conflicting duplicate entries to fail, got nil")
	}

	// Identical duplicate entries must be deterministically validated
	identicalEntries := []PolicyEntry{
		{Fingerprint: validFP, Role: RoleReader},
		{Fingerprint: validFP, Role: RoleReader},
	}
	p, err := NewAuthzPolicyFromEntries(identicalEntries)
	if err != nil {
		t.Fatalf("expected identical duplicate entries to be deterministically validated, got %v", err)
	}
	if role, ok := p.Lookup(validFP); !ok || role != RoleReader {
		t.Fatalf("expected role reader, got %s (ok=%v)", role, ok)
	}
}

// -----------------------------------------------------------------------------
// Bypass attempt tests (12 - 23)
// -----------------------------------------------------------------------------

func TestAuthz_Bypass_RequestCannotSelectDifferentPrincipal(t *testing.T) {
	eng := newCountingMockEngine()

	ca := NewTestCA(t, "ca")
	readerCert, readerKey := ca.IssueClientCert(t, "reader-client")
	readerFP := certFingerprintFromFile(t, readerCert)
	adminFP := "9999999999999999999999999999999999999999999999999999999999999999"

	policy := map[string]string{
		readerFP: "reader",
		adminFP:  "admin",
	}
	addr, cleanup := setupAuthzTestServer(t, ca, policy, eng, nil, false)
	defer cleanup()

	conn := dialClientWithCert(t, addr, ca, readerCert, readerKey)
	defer conn.Close()

	// Request attempting to embed admin fingerprint or user info in key/payload
	req := &Request{
		OpCode: OpPut,
		Key:    []byte("principal=" + adminFP),
		Value:  []byte("role=admin"),
		SeqID:  101,
	}
	_ = WriteRequest(conn, req)
	resp, err := ReadResponse(conn)
	if err != nil {
		t.Fatalf("ReadResponse failed: %v", err)
	}
	if resp.Status != StatusPermissionDenied {
		t.Fatalf("expected StatusPermissionDenied, got 0x%02x", resp.Status)
	}
	if eng.putCount.Load() != 0 {
		t.Fatalf("SECURITY VIOLATION: Put executed despite forged principal payload")
	}
}

func TestAuthz_Bypass_RequestCannotSelectDifferentRole(t *testing.T) {
	eng := newCountingMockEngine()

	ca := NewTestCA(t, "ca")
	readerCert, readerKey := ca.IssueClientCert(t, "reader-client")
	readerFP := certFingerprintFromFile(t, readerCert)

	policy := map[string]string{readerFP: "reader"}
	addr, cleanup := setupAuthzTestServer(t, ca, policy, eng, nil, false)
	defer cleanup()

	conn := dialClientWithCert(t, addr, ca, readerCert, readerKey)
	defer conn.Close()

	// Try sending PUT claiming writer role in value
	req := &Request{
		OpCode: OpPut,
		Key:    []byte("my_key"),
		Value:  []byte(`{"role":"admin"}`),
		SeqID:  102,
	}
	_ = WriteRequest(conn, req)
	resp, _ := ReadResponse(conn)
	if resp.Status != StatusPermissionDenied {
		t.Fatalf("expected StatusPermissionDenied, got 0x%02x", resp.Status)
	}
}

func TestAuthz_Bypass_ChangingFlagsCannotChangeAuthz(t *testing.T) {
	eng := newCountingMockEngine()

	ca := NewTestCA(t, "ca")
	readerCert, readerKey := ca.IssueClientCert(t, "reader-client")
	readerFP := certFingerprintFromFile(t, readerCert)

	policy := map[string]string{readerFP: "reader"}
	addr, cleanup := setupAuthzTestServer(t, ca, policy, eng, nil, false)
	defer cleanup()

	conn := dialClientWithCert(t, addr, ca, readerCert, readerKey)
	defer conn.Close()

	for _, flag := range []byte{FlagSnappy, FlagTracing, 0x04, 0x08, 0xFF} {
		req := &Request{
			OpCode: OpPut,
			Flags:  flag,
			Key:    []byte("k"),
			Value:  []byte("v"),
			SeqID:  uint64(flag) + 1000,
		}
		_ = WriteRequest(conn, req)
		resp, err := ReadResponse(conn)
		if err != nil {
			t.Fatalf("ReadResponse failed for flag 0x%02x: %v", flag, err)
		}
		if resp.Status != StatusPermissionDenied {
			t.Fatalf("expected StatusPermissionDenied for flag 0x%02x, got 0x%02x", flag, resp.Status)
		}
	}
	if eng.putCount.Load() != 0 {
		t.Fatalf("SECURITY VIOLATION: Put executed via altered flags")
	}
}

func TestAuthz_Bypass_ChangingSeqIDCannotChangeAuthz(t *testing.T) {
	eng := newCountingMockEngine()

	ca := NewTestCA(t, "ca")
	readerCert, readerKey := ca.IssueClientCert(t, "reader-client")
	readerFP := certFingerprintFromFile(t, readerCert)

	policy := map[string]string{readerFP: "reader"}
	addr, cleanup := setupAuthzTestServer(t, ca, policy, eng, nil, false)
	defer cleanup()

	conn := dialClientWithCert(t, addr, ca, readerCert, readerKey)
	defer conn.Close()

	for _, seqID := range []uint64{0, 1, 999999, ^uint64(0)} {
		req := &Request{
			OpCode: OpPut,
			Key:    []byte("k"),
			Value:  []byte("v"),
			SeqID:  seqID,
		}
		_ = WriteRequest(conn, req)
		resp, _ := ReadResponse(conn)
		if resp.Status != StatusPermissionDenied || resp.SeqID != seqID {
			t.Fatalf("expected StatusPermissionDenied and SeqID=%d, got status=0x%02x seqID=%d", seqID, resp.Status, resp.SeqID)
		}
	}
}

func TestAuthz_Bypass_PipeliningCannotBypass(t *testing.T) {
	eng := newCountingMockEngine()
	_ = eng.Put(context.Background(), []byte("read1"), []byte("val1"))
	_ = eng.Put(context.Background(), []byte("read2"), []byte("val2"))
	eng.putCount.Store(0)

	ca := NewTestCA(t, "ca")
	readerCert, readerKey := ca.IssueClientCert(t, "reader-client")
	readerFP := certFingerprintFromFile(t, readerCert)

	policy := map[string]string{readerFP: "reader"}
	addr, cleanup := setupAuthzTestServer(t, ca, policy, eng, nil, false)
	defer cleanup()

	conn := dialClientWithCert(t, addr, ca, readerCert, readerKey)
	defer conn.Close()

	// Pipeline: Read, Write (denied), Read, Delete (denied), Exists
	reqs := []*Request{
		{OpCode: OpGet, Key: []byte("read1"), SeqID: 10},
		{OpCode: OpPut, Key: []byte("write1"), Value: []byte("v"), SeqID: 11},
		{OpCode: OpGet, Key: []byte("read2"), SeqID: 12},
		{OpCode: OpDelete, Key: []byte("read1"), SeqID: 13},
		{OpCode: OpExists, Key: []byte("read2"), SeqID: 14},
	}

	for _, r := range reqs {
		if err := WriteRequest(conn, r); err != nil {
			t.Fatalf("WriteRequest failed: %v", err)
		}
	}

	responses := make(map[uint64]*Response)
	for i := 0; i < len(reqs); i++ {
		resp, err := ReadResponse(conn)
		if err != nil {
			t.Fatalf("ReadResponse %d failed: %v", i, err)
		}
		responses[resp.SeqID] = resp
	}

	if responses[10].Status != StatusOk || string(responses[10].Value) != "val1" {
		t.Fatalf("Req 10: expected StatusOk, got %v", responses[10])
	}
	if responses[11].Status != StatusPermissionDenied {
		t.Fatalf("Req 11 (PUT): expected StatusPermissionDenied, got %v", responses[11])
	}
	if responses[12].Status != StatusOk || string(responses[12].Value) != "val2" {
		t.Fatalf("Req 12: expected StatusOk, got %v", responses[12])
	}
	if responses[13].Status != StatusPermissionDenied {
		t.Fatalf("Req 13 (DELETE): expected StatusPermissionDenied, got %v", responses[13])
	}
	if responses[14].Status != StatusOk || !responses[14].Exists {
		t.Fatalf("Req 14: expected StatusOk true, got %v", responses[14])
	}
	if eng.putCount.Load() != 0 {
		t.Fatalf("SECURITY VIOLATION: Put executed under pipelining")
	}
	if eng.deleteCount.Load() != 0 {
		t.Fatalf("SECURITY VIOLATION: Delete executed under pipelining")
	}
}

func TestAuthz_Bypass_ConcurrentRequestsCannotBypass(t *testing.T) {
	eng := newCountingMockEngine()
	_ = eng.Put(context.Background(), []byte("common_key"), []byte("common_val"))
	eng.putCount.Store(0)

	ca := NewTestCA(t, "ca")
	readerCert, readerKey := ca.IssueClientCert(t, "reader-client")
	readerFP := certFingerprintFromFile(t, readerCert)

	policy := map[string]string{readerFP: "reader"}
	addr, cleanup := setupAuthzTestServer(t, ca, policy, eng, nil, false)
	defer cleanup()

	conn := dialClientWithCert(t, addr, ca, readerCert, readerKey)
	defer conn.Close()

	const numReqs = 50
	var writeMu sync.Mutex
	var readMu sync.Mutex

	var wg sync.WaitGroup
	errCh := make(chan error, numReqs)

	for i := 0; i < numReqs; i++ {
		wg.Add(1)
		go func(seq uint64) {
			defer wg.Done()
			var req *Request
			if seq%2 == 0 {
				req = &Request{OpCode: OpGet, Key: []byte("common_key"), SeqID: seq}
			} else {
				req = &Request{OpCode: OpPut, Key: []byte("common_key"), Value: []byte("evil"), SeqID: seq}
			}

			writeMu.Lock()
			err := WriteRequest(conn, req)
			writeMu.Unlock()
			if err != nil {
				errCh <- fmt.Errorf("write error: %w", err)
				return
			}
		}(uint64(i + 1))
	}

	for i := 0; i < numReqs; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			readMu.Lock()
			resp, err := ReadResponse(conn)
			readMu.Unlock()
			if err != nil {
				errCh <- fmt.Errorf("read error: %w", err)
				return
			}
			if resp.SeqID%2 == 0 {
				if resp.Status != StatusOk {
					errCh <- fmt.Errorf("seq %d: expected StatusOk, got 0x%02x", resp.SeqID, resp.Status)
				}
			} else {
				if resp.Status != StatusPermissionDenied {
					errCh <- fmt.Errorf("seq %d: expected StatusPermissionDenied, got 0x%02x", resp.SeqID, resp.Status)
				}
			}
		}()
	}

	wg.Wait()
	close(errCh)
	for err := range errCh {
		t.Fatal(err)
	}
	if eng.putCount.Load() != 0 {
		t.Fatalf("SECURITY VIOLATION: Engine.Put called %d times in concurrent reader test", eng.putCount.Load())
	}
}

func TestAuthz_Adversarial_UnauthorizedWriteNeverCallsEnginePut(t *testing.T) {
	eng := newCountingMockEngine()

	ca := NewTestCA(t, "ca")
	readerCert, readerKey := ca.IssueClientCert(t, "reader")
	policy := map[string]string{certFingerprintFromFile(t, readerCert): "reader"}

	addr, cleanup := setupAuthzTestServer(t, ca, policy, eng, nil, false)
	defer cleanup()

	conn := dialClientWithCert(t, addr, ca, readerCert, readerKey)
	defer conn.Close()

	req := &Request{OpCode: OpPut, Key: []byte("key"), Value: []byte("val"), SeqID: 1}
	_ = WriteRequest(conn, req)
	resp, _ := ReadResponse(conn)

	if resp.Status != StatusPermissionDenied {
		t.Fatalf("expected StatusPermissionDenied, got 0x%02x", resp.Status)
	}
	if eng.putCount.Load() != 0 {
		t.Fatalf("expected engine.Put invocation count == 0, got %d", eng.putCount.Load())
	}
}

func TestAuthz_Adversarial_UnauthorizedDeleteNeverCallsEngineDelete(t *testing.T) {
	eng := newCountingMockEngine()

	ca := NewTestCA(t, "ca")
	readerCert, readerKey := ca.IssueClientCert(t, "reader")
	policy := map[string]string{certFingerprintFromFile(t, readerCert): "reader"}

	addr, cleanup := setupAuthzTestServer(t, ca, policy, eng, nil, false)
	defer cleanup()

	conn := dialClientWithCert(t, addr, ca, readerCert, readerKey)
	defer conn.Close()

	req := &Request{OpCode: OpDelete, Key: []byte("key"), SeqID: 1}
	_ = WriteRequest(conn, req)
	resp, _ := ReadResponse(conn)

	if resp.Status != StatusPermissionDenied {
		t.Fatalf("expected StatusPermissionDenied, got 0x%02x", resp.Status)
	}
	if eng.deleteCount.Load() != 0 {
		t.Fatalf("expected engine.Delete invocation count == 0, got %d", eng.deleteCount.Load())
	}
}

func TestAuthz_Adversarial_UnauthorizedBatchNeverCallsEngineBatch(t *testing.T) {
	eng := newCountingMockEngine()

	ca := NewTestCA(t, "ca")
	readerCert, readerKey := ca.IssueClientCert(t, "reader")
	policy := map[string]string{certFingerprintFromFile(t, readerCert): "reader"}

	addr, cleanup := setupAuthzTestServer(t, ca, policy, eng, nil, false)
	defer cleanup()

	conn := dialClientWithCert(t, addr, ca, readerCert, readerKey)
	defer conn.Close()

	req := &Request{
		OpCode: OpBatch,
		SeqID:  1,
		Batch:  []BatchOp{{Type: BatchOpPut, Key: []byte("k"), Value: []byte("v")}},
	}
	_ = WriteRequest(conn, req)
	resp, _ := ReadResponse(conn)

	if resp.Status != StatusPermissionDenied {
		t.Fatalf("expected StatusPermissionDenied, got 0x%02x", resp.Status)
	}
	if eng.batchCount.Load() != 0 {
		t.Fatalf("expected engine.Batch invocation count == 0, got %d", eng.batchCount.Load())
	}
}

func TestAuthz_Adversarial_UnauthorizedClusteredWriteNeverReachesProposalRouter(t *testing.T) {
	eng := newCountingMockEngine()
	router := &countingMockRouter{}

	ca := NewTestCA(t, "ca")
	readerCert, readerKey := ca.IssueClientCert(t, "reader")
	policy := map[string]string{certFingerprintFromFile(t, readerCert): "reader"}

	addr, cleanup := setupAuthzTestServer(t, ca, policy, eng, router, true)
	defer cleanup()

	conn := dialClientWithCert(t, addr, ca, readerCert, readerKey)
	defer conn.Close()

	req := &Request{OpCode: OpPut, Key: []byte("clustered_key"), Value: []byte("clustered_val"), SeqID: 1}
	_ = WriteRequest(conn, req)
	resp, _ := ReadResponse(conn)

	if resp.Status != StatusPermissionDenied {
		t.Fatalf("expected StatusPermissionDenied, got 0x%02x (%s)", resp.Status, resp.Message)
	}
	if router.writeCount.Load() != 0 {
		t.Fatalf("SECURITY VIOLATION: ProposalRouter.RouteWrite was called %d times for unauthorized client", router.writeCount.Load())
	}
}

func TestAuthz_Adversarial_UnauthorizedClusteredReadNeverReachesReadRouter(t *testing.T) {
	eng := newCountingMockEngine()
	router := &countingMockRouter{}

	ca := NewTestCA(t, "ca")
	unknownCert, unknownKey := ca.IssueClientCert(t, "unknown")
	policy := map[string]string{"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb": "reader"}

	addr, cleanup := setupAuthzTestServer(t, ca, policy, eng, router, true)
	defer cleanup()

	conn := dialClientWithCert(t, addr, ca, unknownCert, unknownKey)
	defer conn.Close()

	req := &Request{OpCode: OpGet, Key: []byte("clustered_key"), SeqID: 1}
	_ = WriteRequest(conn, req)
	resp, _ := ReadResponse(conn)

	if resp.Status != StatusPermissionDenied {
		t.Fatalf("expected StatusPermissionDenied, got 0x%02x (%s)", resp.Status, resp.Message)
	}
	if router.readCount.Load() != 0 {
		t.Fatalf("SECURITY VIOLATION: ReadRouter.RouteRead was called %d times for unauthorized client", router.readCount.Load())
	}
}

func TestAuthz_Adversarial_UnauthorizedStatsRequestNeverReachesCollectStats(t *testing.T) {
	eng := newCountingMockEngine()

	ca := NewTestCA(t, "ca")
	unknownCert, unknownKey := ca.IssueClientCert(t, "unknown")
	policy := map[string]string{"cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc": "reader"}

	addr, cleanup := setupAuthzTestServer(t, ca, policy, eng, nil, false)
	defer cleanup()

	conn := dialClientWithCert(t, addr, ca, unknownCert, unknownKey)
	defer conn.Close()

	req := &Request{OpCode: OpStats, SeqID: 1}
	_ = WriteRequest(conn, req)
	resp, _ := ReadResponse(conn)

	if resp.Status != StatusPermissionDenied {
		t.Fatalf("expected StatusPermissionDenied, got 0x%02x (%s)", resp.Status, resp.Message)
	}
	if eng.statsCount.Load() != 0 {
		t.Fatalf("SECURITY VIOLATION: Stats() was called %d times for unauthorized client", eng.statsCount.Load())
	}
}

// -----------------------------------------------------------------------------
// TLS identity integrity tests (24 - 27)
// -----------------------------------------------------------------------------

func TestAuthz_CertLackingLatticeClientRoleSubjectToPolicy(t *testing.T) {
	eng := newCountingMockEngine()
	ca := NewTestCA(t, "ca")

	// Issue cert without ClientCertRoleOU
	certPath, keyPath := ca.IssueCert(t, "external-app", CertOptions{
		CommonName:   "external-app",
		Organization: []string{"External Org"},
		IsClient:     true,
	})
	fp := certFingerprintFromFile(t, certPath)

	// Configure policy mapping this exact fingerprint to reader
	policy := map[string]string{fp: "reader"}
	addr, cleanup := setupAuthzTestServer(t, ca, policy, eng, nil, false)
	defer cleanup()

	conn := dialClientWithCert(t, addr, ca, certPath, keyPath)
	defer conn.Close()

	// Read should succeed
	reqGet := &Request{OpCode: OpExists, Key: []byte("any"), SeqID: 1}
	_ = WriteRequest(conn, reqGet)
	respGet, _ := ReadResponse(conn)
	if respGet.Status != StatusOk {
		t.Fatalf("expected StatusOk, got 0x%02x", respGet.Status)
	}

	// Write should be denied by RBAC
	reqPut := &Request{OpCode: OpPut, Key: []byte("any"), Value: []byte("val"), SeqID: 2}
	_ = WriteRequest(conn, reqPut)
	respPut, _ := ReadResponse(conn)
	if respPut.Status != StatusPermissionDenied {
		t.Fatalf("expected StatusPermissionDenied for reader, got 0x%02x", respPut.Status)
	}
}

func TestAuthz_RaftPeerCertCannotBecomeAuthorizedClient(t *testing.T) {
	eng := newCountingMockEngine()
	ca := NewTestCA(t, "ca")

	// Issue Raft peer cert
	peerCert, peerKey := ca.IssuePeerCert(t, 2)
	peerFP := certFingerprintFromFile(t, peerCert)

	// Even if an administrator mistakenly added peerFP to the client authorization policy:
	policy := map[string]string{peerFP: "admin"}
	addr, cleanup := setupAuthzTestServer(t, ca, policy, eng, nil, false)
	defer cleanup()

	// Connection must fail during TLS handshake due to PKI separation
	clientCfg, err := ClientTLSConfig(ca.CertPath, peerCert, peerKey, "localhost", false)
	if err != nil {
		t.Fatalf("ClientTLSConfig failed: %v", err)
	}
	conn, err := tls.Dial("tcp", addr, clientCfg)
	if err == nil {
		// If dial returned, write should immediately error or connection is closed
		_ = WriteRequest(conn, &Request{OpCode: OpGet, Key: []byte("k"), SeqID: 1})
		_, readErr := ReadResponse(conn)
		_ = conn.Close()
		if readErr == nil {
			t.Fatalf("SECURITY VIOLATION: Raft peer certificate authenticated as client!")
		}
	}
}

func TestAuthz_DynamicTLSConfigCannotAlterPrincipal(t *testing.T) {
	eng := newCountingMockEngine()
	ca := NewTestCA(t, "ca")

	cert1, key1 := ca.IssueClientCert(t, "client-1")
	fp1 := certFingerprintFromFile(t, cert1)

	policy := map[string]string{fp1: "reader"}

	srvCert, srvKey := ca.IssueServerCert(t, "server")
	cfg := DefaultServerConfig()
	cfg.Address = "127.0.0.1:0"
	cfg.TLSCertFile = srvCert
	cfg.TLSKeyFile = srvKey
	cfg.ClientCAFile = ca.CertPath
	cfg.RequireClientCert = true
	cfg.ClientAuthzPolicy = policy

	srv, err := NewServer(cfg, eng)
	if err != nil {
		t.Fatalf("NewServer failed: %v", err)
	}

	// Install dynamic GetConfigForClient returning another valid config
	baseTLS := srv.cfg.TLSConfig.Clone()
	srv.cfg.TLSConfig.GetConfigForClient = func(chi *tls.ClientHelloInfo) (*tls.Config, error) {
		cloned := baseTLS.Clone()
		return cloned, nil
	}

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Listen failed: %v", err)
	}
	go func() { _ = srv.Serve(ln) }()
	defer func() {
		_ = srv.Shutdown(context.Background())
		_ = ln.Close()
	}()

	conn := dialClientWithCert(t, ln.Addr().String(), ca, cert1, key1)
	defer conn.Close()

	// Should be authorized as reader
	req := &Request{OpCode: OpExists, Key: []byte("k"), SeqID: 1}
	_ = WriteRequest(conn, req)
	resp, err := ReadResponse(conn)
	if err != nil {
		t.Fatalf("ReadResponse failed: %v", err)
	}
	if resp.Status != StatusOk {
		t.Fatalf("expected StatusOk, got 0x%02x", resp.Status)
	}

	// PUT should still be denied as reader
	reqPut := &Request{OpCode: OpPut, Key: []byte("k"), Value: []byte("v"), SeqID: 2}
	_ = WriteRequest(conn, reqPut)
	respPut, _ := ReadResponse(conn)
	if respPut.Status != StatusPermissionDenied {
		t.Fatalf("expected StatusPermissionDenied, got 0x%02x", respPut.Status)
	}
}

func TestAuthz_ReplacingCertRequiresNewConnection(t *testing.T) {
	eng := newCountingMockEngine()
	ca := NewTestCA(t, "ca")

	readerCert, readerKey := ca.IssueClientCert(t, "reader")
	writerCert, writerKey := ca.IssueClientCert(t, "writer")
	readerFP := certFingerprintFromFile(t, readerCert)
	writerFP := certFingerprintFromFile(t, writerCert)

	policy := map[string]string{
		readerFP: "reader",
		writerFP: "writer",
	}
	addr, cleanup := setupAuthzTestServer(t, ca, policy, eng, nil, false)
	defer cleanup()

	// Connect as reader
	conn := dialClientWithCert(t, addr, ca, readerCert, readerKey)
	defer conn.Close()

	// Over this connection, all PUTs are denied
	req := &Request{OpCode: OpPut, Key: []byte("k"), Value: []byte("v"), SeqID: 1}
	_ = WriteRequest(conn, req)
	resp, _ := ReadResponse(conn)
	if resp.Status != StatusPermissionDenied {
		t.Fatalf("expected StatusPermissionDenied on reader connection")
	}

	// To use writer privileges, client MUST establish a new authenticated TLS connection
	connWriter := dialClientWithCert(t, addr, ca, writerCert, writerKey)
	defer connWriter.Close()

	_ = WriteRequest(connWriter, req)
	respWriter, _ := ReadResponse(connWriter)
	if respWriter.Status != StatusOk {
		t.Fatalf("expected StatusOk on new writer connection, got 0x%02x", respWriter.Status)
	}
}

// -----------------------------------------------------------------------------
// Lifecycle/concurrency tests (28 - 30)
// -----------------------------------------------------------------------------

func TestAuthz_MultipleSimultaneousConnectionsSameCert(t *testing.T) {
	eng := newCountingMockEngine()
	_ = eng.Put(context.Background(), []byte("common"), []byte("val"))

	ca := NewTestCA(t, "ca")
	readerCert, readerKey := ca.IssueClientCert(t, "shared-reader")
	readerFP := certFingerprintFromFile(t, readerCert)

	policy := map[string]string{readerFP: "reader"}
	addr, cleanup := setupAuthzTestServer(t, ca, policy, eng, nil, false)
	defer cleanup()

	const numConns = 10
	var wg sync.WaitGroup
	errCh := make(chan error, numConns)

	for i := 0; i < numConns; i++ {
		wg.Add(1)
		go func(connIdx int) {
			defer wg.Done()
			conn := dialClientWithCert(t, addr, ca, readerCert, readerKey)
			defer conn.Close()

			for j := 0; j < 5; j++ {
				req := &Request{OpCode: OpGet, Key: []byte("common"), SeqID: uint64(connIdx*100 + j)}
				if err := WriteRequest(conn, req); err != nil {
					errCh <- err
					return
				}
				resp, err := ReadResponse(conn)
				if err != nil {
					errCh <- err
					return
				}
				if resp.Status != StatusOk {
					errCh <- fmt.Errorf("conn %d req %d: expected StatusOk, got 0x%02x", connIdx, j, resp.Status)
					return
				}
			}
		}(i)
	}

	wg.Wait()
	close(errCh)
	for err := range errCh {
		t.Fatal(err)
	}
}

func TestAuthz_SimultaneousReaderAndWriterRaceFree(t *testing.T) {
	eng := newCountingMockEngine()
	ca := NewTestCA(t, "ca")

	readerCert, readerKey := ca.IssueClientCert(t, "concurrent-reader")
	writerCert, writerKey := ca.IssueClientCert(t, "concurrent-writer")
	readerFP := certFingerprintFromFile(t, readerCert)
	writerFP := certFingerprintFromFile(t, writerCert)

	policy := map[string]string{
		readerFP: "reader",
		writerFP: "writer",
	}
	addr, cleanup := setupAuthzTestServer(t, ca, policy, eng, nil, false)
	defer cleanup()

	const workers = 8
	const opsPerWorker = 30
	var wg sync.WaitGroup
	errCh := make(chan error, workers*2)

	// Spawn reader workers
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			conn := dialClientWithCert(t, addr, ca, readerCert, readerKey)
			defer conn.Close()

			for i := 0; i < opsPerWorker; i++ {
				req := &Request{OpCode: OpExists, Key: []byte(fmt.Sprintf("k-%d", i)), SeqID: uint64(id*1000 + i)}
				if err := WriteRequest(conn, req); err != nil {
					errCh <- err
					return
				}
				resp, err := ReadResponse(conn)
				if err != nil {
					errCh <- err
					return
				}
				if resp.Status != StatusOk {
					errCh <- fmt.Errorf("reader worker %d: expected StatusOk, got 0x%02x", id, resp.Status)
					return
				}
			}
		}(w)
	}

	// Spawn writer workers
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			conn := dialClientWithCert(t, addr, ca, writerCert, writerKey)
			defer conn.Close()

			for i := 0; i < opsPerWorker; i++ {
				req := &Request{
					OpCode: OpPut,
					Key:    []byte(fmt.Sprintf("k-%d", i)),
					Value:  []byte(fmt.Sprintf("v-%d-%d", id, i)),
					SeqID:  uint64(id*1000 + i),
				}
				if err := WriteRequest(conn, req); err != nil {
					errCh <- err
					return
				}
				resp, err := ReadResponse(conn)
				if err != nil {
					errCh <- err
					return
				}
				if resp.Status != StatusOk {
					errCh <- fmt.Errorf("writer worker %d: expected StatusOk, got 0x%02x", id, resp.Status)
					return
				}
			}
		}(workers + w)
	}

	wg.Wait()
	close(errCh)
	for err := range errCh {
		t.Fatal(err)
	}
}

func TestAuthz_ServerShutdownWithAuthzInFlight(t *testing.T) {
	eng := newCountingMockEngine()
	ca := NewTestCA(t, "ca")

	writerCert, writerKey := ca.IssueClientCert(t, "shutdown-writer")
	writerFP := certFingerprintFromFile(t, writerCert)

	policy := map[string]string{writerFP: "writer"}
	srvCert, srvKey := ca.IssueServerCert(t, "server")

	cfg := DefaultServerConfig()
	cfg.Address = "127.0.0.1:0"
	cfg.TLSCertFile = srvCert
	cfg.TLSKeyFile = srvKey
	cfg.ClientCAFile = ca.CertPath
	cfg.RequireClientCert = true
	cfg.ClientAuthzPolicy = policy
	cfg.ShutdownTimeout = 2 * time.Second

	srv, err := NewServer(cfg, eng)
	if err != nil {
		t.Fatalf("NewServer failed: %v", err)
	}

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Listen failed: %v", err)
	}

	go func() { _ = srv.Serve(ln) }()

	conn := dialClientWithCert(t, ln.Addr().String(), ca, writerCert, writerKey)
	defer conn.Close()

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 50; i++ {
			req := &Request{OpCode: OpPut, Key: []byte("key"), Value: []byte("val"), SeqID: uint64(i + 1)}
			if err := WriteRequest(conn, req); err != nil {
				return
			}
			if _, err := ReadResponse(conn); err != nil {
				return
			}
		}
	}()

	time.Sleep(5 * time.Millisecond)
	_ = srv.Shutdown(context.Background())
	_ = ln.Close()

	wg.Wait()
}
