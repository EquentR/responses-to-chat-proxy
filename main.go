package main

import (
	"flag"
	"log"
	"os"

	"github.com/EquentR/responses-to-chat-proxy/internal/proxy"
)

func main() {
	interactive := flag.Bool("interactive", false, "run the interactive local launcher")
	flag.Parse()

	var (
		cfg proxy.Config
		err error
	)

	if *interactive {
		cfg, err = proxy.RunInteractiveMode(os.Stdout, proxy.NewConsolePrompter(os.Stdin, os.Stdout))
	} else {
		cfg, err = proxy.LoadConfigFromEnv(".env")
	}
	if err != nil {
		log.Fatal(err)
	}

	if err := proxy.Run(cfg); err != nil {
		log.Fatal(err)
	}
}
