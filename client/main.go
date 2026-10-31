package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"math/rand"
	"net/http"
	"os"
	"sort"
	"sync"
	"time"

	pb "sis2/proto/taskv1"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/protobuf/proto"
)

const (
	concurrency       = 50
	requestsPerWorker = 50
	warmupCalls       = 200
)

func getEnv(key, defaultVal string) string {
	if val := os.Getenv(key); val != "" {
		return val
	}
	return defaultVal
}

func tlsVersionName(version uint16) string {
	switch version {
	case tls.VersionTLS13:
		return "TLS 1.3"
	case tls.VersionTLS12:
		return "TLS 1.2"
	default:
		return fmt.Sprintf("Unknown (%x)", version)
	}
}

func retryWithBackoff(attempts int, initialBackoff time.Duration, fn func() error) error {
	var err error
	for i := 0; i < attempts; i++ {
		err = fn()
		if err == nil {
			return nil
		}
		if i == attempts-1 {
			break
		}
		maxBackoff := initialBackoff * (1 << i)
		jitter := time.Duration(rand.Int63n(int64(maxBackoff)))
		time.Sleep(jitter)
	}
	return err
}

func main() {
	totalCalls := concurrency * requestsPerWorker

	grpcHost := getEnv("GRPC_HOST", "localhost:50051")
	restH2Base := getEnv("REST_H2_URL", "https://localhost:8443/api/v1/tasks")
	restH1Base := getEnv("REST_H1_URL", "https://localhost:8443/api/v1/tasks")

	strictTLS13 := &tls.Config{
		InsecureSkipVerify: true,
		MinVersion: tls.VersionTLS13,
		MaxVersion: tls.VersionTLS13,
	}

	// 1. gRPC HTTP/2
	grpcCreds := credentials.NewTLS(strictTLS13)
	grpcConn, err := grpc.NewClient(grpcHost, grpc.WithTransportCredentials(grpcCreds))
	if err != nil {
		log.Fatalf("gRPC connection failed: %v", err)
	}
	defer grpcConn.Close()
	grpcClient := pb.NewTaskServiceClient(grpcConn)

	// 2. REST HTTP/2
	http2Client := &http.Client{
		Transport: &http.Transport{
			TLSClientConfig:   strictTLS13.Clone(),
			ForceAttemptHTTP2: true,
		},
		Timeout: 3 * time.Second,
	}

	// 3. REST HTTP/1.1
	http1Client := &http.Client{
		Transport: &http.Transport{
			TLSClientConfig:     strictTLS13.Clone(),
			TLSNextProto:        make(map[string]func(authority string, c *tls.Conn) http.RoundTripper),
			MaxIdleConns:        100,
			MaxIdleConnsPerHost: 50,
			IdleConnTimeout:     90 * time.Second,
		},
		Timeout: 3 * time.Second,
	}

	// Pre-calculate payload wire sizes
	singleTask := &pb.CreateTaskRequest{
		IdempotencyKey: "019294a2-7b56-782a-bcf0-234b98124000",
		Title:          "Production HighLoad Protocol Task",
		Description:    "Benchmarking wire size and latency distribution",
		Priority:       3,
	}
	singleProto, _ := proto.Marshal(singleTask)
	singleJSON, _ := json.Marshal(map[string]interface{}{
		"title":       singleTask.Title,
		"description": singleTask.Description,
		"priority":    singleTask.Priority,
	})

	// Pre-seed read for bulk measurement
	resList, _ := grpcClient.ListTasks(context.Background(), &pb.ListTasksRequest{Limit: 50})
	bulkProto, _ := proto.Marshal(resList)
	bulkJSON, _ := json.Marshal(resList.GetTasks())

	fmt.Println("")
	fmt.Println("SIS #2 BENCHMARK: gRPC (TLS 1.3) vs REST H2 vs REST H1.1 (TLS 1.3)")
	fmt.Println("")
	fmt.Printf("[Payload Wire Size]\n")
	fmt.Printf("Single Object   -> Protobuf: %4d bytes | JSON: %4d bytes\n", len(singleProto), len(singleJSON))
	if len(bulkJSON) > 0 {
		savings := float64(len(bulkJSON)-len(bulkProto)) / float64(len(bulkJSON)) * 100.0
		fmt.Printf("List (50 Items) -> Protobuf: %4d bytes | JSON: %4d bytes (Savings: %.1f%%)\n\n", len(bulkProto), len(bulkJSON), savings)
	}

	// Assert ALPN negotiation
	probeH2, errH2 := http2Client.Post(restH2Base, "application/json", bytes.NewReader(singleJSON))
	if errH2 == nil {
		fmt.Printf("  REST H2 Target:  %s | Protocol: %s | Security: %s\n", restH2Base, probeH2.Proto, tlsVersionName(probeH2.TLS.Version))
		probeH2.Body.Close()
	}
	probeH1, errH1 := http1Client.Post(restH1Base, "application/json", bytes.NewReader(singleJSON))
	if errH1 == nil {
		fmt.Printf("  REST H1 Target:  %s | Protocol: %s | Security: %s\n\n", restH1Base, probeH1.Proto, tlsVersionName(probeH1.TLS.Version))
		probeH1.Body.Close()
	}

	// Warmup
	for i := 0; i < warmupCalls; i++ {
		ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
		_, _ = grpcClient.GetTask(ctx, &pb.GetTaskRequest{TaskId: "task-0001"})
		cancel()
	}

	// 1. Point Read (GetTask)
	fmt.Println("[Point Read: GetTask]")
	runBenchmark("gRPC GetTask (HTTP/2 / TLS 1.3)", totalCalls, func() {
		_ = retryWithBackoff(3, 5*time.Millisecond, func() error {
			ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
			defer cancel()
			_, err := grpcClient.GetTask(ctx, &pb.GetTaskRequest{TaskId: "task-0001"})
			return err
		})
	})

	runBenchmark("REST H2 GetTask (HTTP/2 / TLS 1.3)", totalCalls, func() {
		_ = retryWithBackoff(3, 5*time.Millisecond, func() error {
			resp, err := http2Client.Get(restH2Base + "/task-0001")
			if err != nil {
				return err
			}
			_, _ = io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
			return nil
		})
	})

	runBenchmark("REST H1.1 GetTask (HTTP/1.1 / TLS 1.3)", totalCalls, func() {
		_ = retryWithBackoff(3, 5*time.Millisecond, func() error {
			resp, err := http1Client.Get(restH1Base + "/task-0001")
			if err != nil {
				return err
			}
			_, _ = io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
			return nil
		})
	})

	// 2. Collection Read (ListTasks - 50 items)
	fmt.Println("[Bulk Read: ListTasks (50 items)]")
	runBenchmark("gRPC ListTasks (HTTP/2 / TLS 1.3)", totalCalls, func() {
		_ = retryWithBackoff(3, 5*time.Millisecond, func() error {
			ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
			defer cancel()
			_, err := grpcClient.ListTasks(ctx, &pb.ListTasksRequest{Limit: 50})
			return err
		})
	})

	runBenchmark("REST H2 ListTasks (HTTP/2 / TLS 1.3)", totalCalls, func() {
		_ = retryWithBackoff(3, 5*time.Millisecond, func() error {
			resp, err := http2Client.Get(restH2Base + "?limit=50")
			if err != nil {
				return err
			}
			_, _ = io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
			return nil
		})
	})

	runBenchmark("REST H1.1 ListTasks (HTTP/1.1 / TLS 1.3)", totalCalls, func() {
		_ = retryWithBackoff(3, 5*time.Millisecond, func() error {
			resp, err := http1Client.Get(restH1Base + "?limit=50")
			if err != nil {
				return err
			}
			_, _ = io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
			return nil
		})
	})

	// 3. Write (CreateTask)
	fmt.Println("[Write: CreateTask]")
	runBenchmark("gRPC CreateTask (HTTP/2 / TLS 1.3)", totalCalls, func() {
		_ = retryWithBackoff(3, 5*time.Millisecond, func() error {
			ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
			defer cancel()
			_, err := grpcClient.CreateTask(ctx, singleTask)
			return err
		})
	})

	runBenchmark("REST H2 CreateTask (HTTP/2 / TLS 1.3)", totalCalls, func() {
		_ = retryWithBackoff(3, 5*time.Millisecond, func() error {
			req, _ := http.NewRequest("POST", restH2Base, bytes.NewReader(singleJSON))
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("Idempotency-Key", singleTask.IdempotencyKey)
			resp, err := http2Client.Do(req)
			if err != nil {
				return err
			}
			_, _ = io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
			return nil
		})
	})

	runBenchmark("REST H1.1 CreateTask (HTTP/1.1 / TLS 1.3)", totalCalls, func() {
		_ = retryWithBackoff(3, 5*time.Millisecond, func() error {
			req, _ := http.NewRequest("POST", restH1Base, bytes.NewReader(singleJSON))
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("Idempotency-Key", singleTask.IdempotencyKey)
			resp, err := http1Client.Do(req)
			if err != nil {
				return err
			}
			_, _ = io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
			return nil
		})
	})

	fmt.Println("End of the tests")
}

func runBenchmark(label string, totalCalls int, op func()) {
	latencies := make([]int64, totalCalls)
	var wg sync.WaitGroup
	start := time.Now()

	for w := 0; w < concurrency; w++ {
		wg.Add(1)
		workerIndex := w
		go func() {
			defer wg.Done()
			offset := workerIndex * requestsPerWorker
			for i := 0; i < requestsPerWorker; i++ {
				t0 := time.Now()
				op()
				latencies[offset+i] = time.Since(t0).Microseconds()
			}
		}()
	}

	wg.Wait()
	duration := time.Since(start)
	rps := float64(totalCalls) / duration.Seconds()

	sort.Slice(latencies, func(i, j int) bool { return latencies[i] < latencies[j] })
	p50 := latencies[int(float64(totalCalls)*0.50)]
	p95 := latencies[int(float64(totalCalls)*0.95)]
	p99 := latencies[int(float64(totalCalls)*0.99)]

	fmt.Printf("[%s]\n", label)
	fmt.Printf("  Achieved RPS: %8.0f requests/sec\n", rps)
	fmt.Printf("  p50 Latency:  %6d µs (%.3f ms)\n", p50, float64(p50)/1000.0)
	fmt.Printf("  p95 Latency:  %6d µs (%.3f ms)\n", p95, float64(p95)/1000.0)
	fmt.Printf("  p99 Latency:  %6d µs (%.3f ms)\n\n", p99, float64(p99)/1000.0)
}