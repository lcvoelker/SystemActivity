package main

import (
	"fmt"
	"log"
	"os"
	"time"

	"main/internal/session"
	"main/internal/watcher"
)

func main() {

	var root string
	if len(os.Args) < 2 {
		log.Fatal("usage: tracker <directory>")
	}

	if os.Args[1] == "--" {
		if len(os.Args) < 3 {
			log.Fatal("usage: tracker <directory>")
		}
		root = os.Args[2]
	} else {
		root = os.Args[1]
	}

	wd, err := os.Getwd()
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println("Working directory: ", wd)
	fmt.Println("Watching: ", root)
	fmt.Println("Args: ", os.Args)
	w, err := watcher.New()
	if err != nil {
		log.Fatal(err)
	}

	if err := w.AddRecursive(root); err != nil {
		log.Fatal(err)
	}

	manager := session.NewManager(5 * time.Minute)
	events := make(chan session.Event, 100) //buffered channel
	// start watcher and pass it the channel
	go w.Start(events)
	//go watcher.Start(events)

	// consume events
	for e := range events {
		fmt.Printf("EVENT: %s %s\n", e.Op, e.Path)
		manager.AddEvent(e)
	}

}
