package main

import (
	"fmt"
	"io"
	"log"
	"net/http"
	"time"
)

func postHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		fmt.Fprintln(w, "Method not allowed")
		return
	}

	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "Error reading request body", http.StatusBadRequest)
		return
	}
	defer r.Body.Close()

	fmt.Println("Received POST request body:", string(body)) // log request body on server side

	w.WriteHeader(http.StatusOK)
	fmt.Fprintln(w, "POST request received and processed")
}

func main() {
	mux := http.NewServeMux()            // create ServeMux
	mux.HandleFunc("/post", postHandler) // register handler to ServeMux
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		fmt.Fprintln(w, "Hello, World!")
	})

	server := &http.Server{ // create http.Server
		Addr:           ":8080",
		Handler:        mux,              // set ServeMux to Handler
		ReadTimeout:    15 * time.Second, // set ReadTimeout
		WriteTimeout:   15 * time.Second, // set WriteTimeout
		IdleTimeout:    60 * time.Second, // set IdleTimeout
		MaxHeaderBytes: 1 << 20,          // set MaxHeaderBytes (optional)
	}

	fmt.Println("Server listening on :8080")
	log.Fatal(server.ListenAndServe()) // use server.ListenAndServe()
}
