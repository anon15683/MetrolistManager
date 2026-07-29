package main

import (
	"errors"
	"fmt"
	"os"

	"github.com/MetrolistGroup/MetrolistManager/internal/payload"
)

func main() {
	if len(os.Args) != 3 {
		fmt.Fprintln(os.Stderr, "usage: validate-payload <archive.zip> <version>")
		os.Exit(2)
	}
	archive, _, err := payload.Open(os.Args[1], os.Args[2])
	if err == nil {
		err = archive.Verify()
	}
	if archive != nil {
		err = errors.Join(err, archive.Close())
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	fmt.Println("payload is valid")
}
