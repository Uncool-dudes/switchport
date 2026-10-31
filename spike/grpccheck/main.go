// grpccheck drives real grpc-go through a running switchport daemon, using the
// reflection service because it is a built-in bidirectional stream.
//
// usage: grpccheck serve <addr>
//
//	grpccheck call <proxy-addr> <server-name> <ca.pem>
package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"log"
	"net"
	"os"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/reflection"
	refl "google.golang.org/grpc/reflection/grpc_reflection_v1"
)

func main() {
	switch os.Args[1] {
	case "serve":
		ln, err := net.Listen("tcp", os.Args[2])
		if err != nil {
			log.Fatal(err)
		}
		s := grpc.NewServer()
		reflection.Register(s)
		log.Fatal(s.Serve(ln))
	case "call":
		call(os.Args[2], os.Args[3], os.Args[4])
	}
}

func call(addr, name, caPath string) {
	pem, err := os.ReadFile(caPath)
	if err != nil {
		log.Fatal(err)
	}
	roots := x509.NewCertPool()
	roots.AppendCertsFromPEM(pem)
	creds := credentials.NewTLS(&tls.Config{RootCAs: roots, ServerName: name})
	conn, err := grpc.NewClient("passthrough:///"+addr, grpc.WithTransportCredentials(creds), grpc.WithAuthority(name))
	if err != nil {
		log.Fatal(err)
	}
	defer conn.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	stream, err := refl.NewServerReflectionClient(conn).ServerReflectionInfo(ctx)
	if err != nil {
		log.Fatal(err)
	}
	// Each reply must arrive before the next send, which only full duplex allows.
	const rounds = 25
	for i := range rounds {
		req := &refl.ServerReflectionRequest{MessageRequest: &refl.ServerReflectionRequest_ListServices{}}
		if err := stream.Send(req); err != nil {
			log.Fatalf("send %d: %v", i, err)
		}
		res, err := stream.Recv()
		if err != nil {
			log.Fatalf("recv %d: %v", i, err)
		}
		if res.GetListServicesResponse() == nil {
			log.Fatalf("recv %d: unexpected %v", i, res)
		}
	}
	if err := stream.CloseSend(); err != nil {
		log.Fatal(err)
	}
	fmt.Printf("bidi ok: %d round trips\n", rounds)
}
