package main

import (
	"fmt"
	"log"
	"os"

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

	fmt.Println("Args: ", os.Args)
	w, err := watcher.New()
	if err != nil {
		log.Fatal(err)
	}

	if err := w.AddRecursive(root); err != nil {
		log.Fatal(err)
	}

	w.Start()

	fmt.Println("Watching: ", root)
	for ev := range w.Out {
		fmt.Printf("[%v] %s\n", ev.Type, ev.Path)
	}
}
