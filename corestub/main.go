package main

import (
	"context"
	"log"
	"os"

	"github.com/equinor/seismic-cloud-api/api/service/store"
	"github.com/equinor/seismic-cloud-api/corestub/server"
)

func main() {

	hostAddr := os.Getenv("SC_GRPC_HOST_ADDR")
	if len(hostAddr) < 1 {
		hostAddr = "localhost:10000"
	}

	var err error
	ss, err := store.NewSurfaceStore(surfaceStoreConfig())
	if err != nil {
		log.Fatalf("No surface store, error: %v", err)
	}
	log.Fatalf(server.StartServer(context.Background(), hostAddr, ss))
}

func surfaceStoreConfig() interface{} {

	if len(os.Getenv("AZURE_STORAGE_ACCOUNT")) > 0 && len(os.Getenv("AZURE_STORAGE_ACCESS_KEY")) > 0 {
		return store.AzureBlobSettings{
			AccountName:   os.Getenv("AZURE_STORAGE_ACCOUNT"),
			AccountKey:    os.Getenv("AZURE_STORAGE_ACCESS_KEY"),
			ContainerName: "scblob",
		}
	}

	if len(os.Getenv("LOCAL_SURFACE_PATH")) > 0 {
		return store.BasePath(os.Getenv("LOCAL_SURFACE_PATH"))
	}

	return store.BasePath("/tmp/")

}
