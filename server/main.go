package main

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"math/big"
	"net"
	"net/http"
	"strconv"
	"sync"
	"time"

	pb "sis2/proto/taskv1"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/status"
)

type TaskServer struct {
	pb.UnimplementedTaskServiceServer
	mu sync.RWMutex
	tasks map[string]*pb.TaskResponse
	idempStore map[string]*pb.TaskResponse
}

func newTaskServer() *TaskServer {
	s := &TaskServer{
		tasks: make(map[string]*pb.TaskResponse),
		idempStore: make(map[string]*pb.TaskResponse),
	}
	// Pre-seed 50 tasks for collection benchmark testing
	for i := 1; i <= 50; i++ {
		id := fmt.Sprintf("task-%04d", i)
		s.tasks[id] = &pb.TaskResponse{
			TaskId: id,
			Title: fmt.Sprintf("Pre-seeded Benchmark Task #%d", i),
			Description: "Highload environment task tracker benchmark payload item",
			Priority: int32(i % 5),
			Status: 0, // TODO
			Version: 1,
			CreatedAt: time.Now().Unix(),
		}
	}
	return s
}

// gRPC
func (s *TaskServer) CreateTask(ctx context.Context, req *pb.CreateTaskRequest) (*pb.TaskResponse, error) {
	if err := ctx.Err(); err != nil {
		return nil, status.Error(codes.DeadlineExceeded, "deadline exceeded")
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	if req.IdempotencyKey != "" {
		if cached, ok := s.idempStore[req.IdempotencyKey]; ok {
			return cached, nil
		}
	}

	taskID := randomID(8)
	task := &pb.TaskResponse{
		TaskId: taskID,
		Title: req.Title,
		Description: req.Description,
		Priority: req.Priority,
		Status: 0,
		Version: 1,
		CreatedAt: time.Now().Unix(),
	}
	s.tasks[taskID] = task
	if req.IdempotencyKey != "" {
		s.idempStore[req.IdempotencyKey] = task
	}
	return task, nil
}

func (s *TaskServer) GetTask(ctx context.Context, req *pb.GetTaskRequest) (*pb.TaskResponse, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	task, ok := s.tasks[req.TaskId]
	if !ok {
		return nil, status.Error(codes.NotFound, "task not found")
	}
	return task, nil
}

func (s *TaskServer) ListTasks(ctx context.Context, req *pb.ListTasksRequest) (*pb.ListTasksResponse, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	limit := int(req.Limit)
	if limit <= 0 || limit > len(s.tasks) {
		limit = 50
	}

	list := make([]*pb.TaskResponse, 0, limit)
	count := 0
	for _, task := range s.tasks {
		if count >= limit {
			break
		}
		list = append(list, task)
		count++
	}
	return &pb.ListTasksResponse{Tasks: list}, nil
}

func (s *TaskServer) UpdateTask(ctx context.Context, req *pb.UpdateTaskRequest) (*pb.TaskResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	task, ok := s.tasks[req.TaskId]
	if !ok {
		return nil, status.Error(codes.NotFound, "task not found")
	}

	// Optimistic concurrency check
	if task.Version != req.Version {
		return nil, status.Error(codes.Aborted, "optimistic lock error: version mismatch")
	}

	task.Status = req.Status
	task.Version++
	if req.Title != "" {
		task.Title = req.Title
	}
	if req.Description != "" {
		task.Description = req.Description
	}
	return task, nil
}

func (s *TaskServer) DeleteTask(ctx context.Context, req *pb.DeleteTaskRequest) (*pb.DeleteTaskResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	delete(s.tasks, req.TaskId)
	return &pb.DeleteTaskResponse{Success: true}, nil
}

// Rest
func (s *TaskServer) registerRestRoutes(mux *http.ServeMux) {
	// POST /api/v1/tasks & GET /api/v1/tasks
	mux.HandleFunc("/api/v1/tasks", func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodPost:
			idempKey := r.Header.Get("Idempotency-Key")
			var req struct {
				Title string `json:"title"`
				Description string `json:"description"`
				Priority int32 `json:"priority"`
			}
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			task, _ := s.CreateTask(r.Context(), &pb.CreateTaskRequest{
				IdempotencyKey: idempKey,
				Title: req.Title,
				Description: req.Description,
				Priority: req.Priority,
			})
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(task)

		case http.MethodGet:
			limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
			res, _ := s.ListTasks(r.Context(), &pb.ListTasksRequest{Limit: int32(limit)})
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(res.Tasks)

		default:
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		}
	})

	// GET, PATCH, DELETE /api/v1/tasks/
	mux.HandleFunc("/api/v1/tasks/", func(w http.ResponseWriter, r *http.Request) {
		id := r.URL.Path[len("/api/v1/tasks/"):]
		if id == "" {
			http.Error(w, "missing task id", http.StatusBadRequest)
			return
		}

		switch r.Method {
		case http.MethodGet:
			task, err := s.GetTask(r.Context(), &pb.GetTaskRequest{TaskId: id})
			if err != nil {
				http.Error(w, "not found", http.StatusNotFound)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(task)

		case http.MethodPatch:
			var req struct {
				Status int32 `json:"status"`
				Version int32 `json:"version"`
				Title string `json:"title"`
				Description string `json:"description"`
			}
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			task, err := s.UpdateTask(r.Context(), &pb.UpdateTaskRequest{
				TaskId: id,
				Status: req.Status,
				Version: req.Version,
				Title: req.Title,
				Description: req.Description,
			})
			if err != nil {
				http.Error(w, "conflict or not found", http.StatusConflict)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(task)

		case http.MethodDelete:
			_, _ = s.DeleteTask(r.Context(), &pb.DeleteTaskRequest{TaskId: id})
			w.WriteHeader(http.StatusNoContent)

		default:
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		}
	})
}

func randomID(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func generateEphemeralCert() (tls.Certificate, error) {
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return tls.Certificate{}, err
	}
	template := x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject: pkix.Name{Organization: []string{"HighLoad SIS2"}},
		NotBefore: time.Now(),
		NotAfter: time.Now().Add(24 * time.Hour),
		KeyUsage: x509.KeyUsageKeyEncipherment | x509.KeyUsageDigitalSignature,
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		IPAddresses: []net.IP{net.ParseIP("127.0.0.1"), net.IPv6loopback},
		DNSNames: []string{"localhost", "task-service"},
	}
	derBytes, err := x509.CreateCertificate(rand.Reader, &template, &template, &priv.PublicKey, priv)
	if err != nil {
		return tls.Certificate{}, err
	}
	return tls.Certificate{Certificate: [][]byte{derBytes}, PrivateKey: priv}, nil
}

func main() {
	srv := newTaskServer()
	cert, err := generateEphemeralCert()
	if err != nil {
		log.Fatalf("cert generation failed: %v", err)
	}

	baseTLS := &tls.Config{
		Certificates: []tls.Certificate{cert},
		MinVersion:   tls.VersionTLS13,
		MaxVersion:   tls.VersionTLS13,
	}

	// 1. gRPC Server on :50051 (HTTP/2 / TLS 1.3)
	grpcTLS := baseTLS.Clone()
	grpcTLS.NextProtos = []string{"h2"}
	grpcLis, err := tls.Listen("tcp", ":50051", grpcTLS)
	if err != nil {
		log.Fatalf("failed to listen on :50051: %v", err)
	}
	grpcServer := grpc.NewServer(grpc.Creds(credentials.NewTLS(grpcTLS)))
	pb.RegisterTaskServiceServer(grpcServer, srv)
	go func() {
		log.Println("[INFO] gRPC server running on :50051 (Protobuf / HTTP-2 / TLS 1.3)")
		if err := grpcServer.Serve(grpcLis); err != nil {
			log.Fatalf("grpc serve failed: %v", err)
		}
	}()

	// 2. HTTPS REST Server on :8443 (ALPN: HTTP/2 & HTTP/1.1 / TLS 1.3)
	mux := http.NewServeMux()
	srv.registerRestRoutes(mux)

	restTLS := baseTLS.Clone()
	restTLS.NextProtos = []string{"h2", "http/1.1"}
	restLis, err := tls.Listen("tcp", ":8443", restTLS)
	if err != nil {
		log.Fatalf("failed to listen on :8443: %v", err)
	}
	go func() {
		log.Println("[INFO] HTTPS REST server running on :8443 (ALPN: HTTP/2 & HTTP/1.1 / TLS 1.3)")
		server := &http.Server{Handler: mux}
		if err := server.Serve(restLis); err != nil {
			log.Fatalf("https serve failed: %v", err)
		}
	}()

	// 3. Plaintext REST Baseline on :8080
	log.Println("[INFO] Plaintext REST baseline running on :8080 (Cleartext HTTP/1.1)")
	if err := http.ListenAndServe(":8080", mux); err != nil {
		log.Fatalf("rest baseline serve failed: %v", err)
	}
}
