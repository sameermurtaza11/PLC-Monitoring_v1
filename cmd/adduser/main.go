// adduser creates a login account. Use it to create the FIRST administrator
// (there is deliberately no default account), or when you cannot log in.
//
//	go run ./cmd/adduser -user sameer -role admin
//
// The password is never a command-line argument (it would end up in shell
// history and the process list). It is read from the PLC_USER_PASSWORD
// environment variable, or typed on stdin when that is not set.
package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"PLC_Monitoring/internal/auth"
	"PLC_Monitoring/internal/config"
	"PLC_Monitoring/internal/store"
)

func main() {
	user := flag.String("user", "", "username (required)")
	role := flag.String("role", "", "operator | supervisor | admin (required)")
	flag.Parse()
	if *user == "" || *role == "" {
		fmt.Fprintln(os.Stderr, "usage: go run ./cmd/adduser -user NAME -role operator|supervisor|admin")
		os.Exit(2)
	}
	if !auth.Role(*role).Valid() {
		fmt.Fprintln(os.Stderr, "role must be operator, supervisor or admin")
		os.Exit(2)
	}

	password := os.Getenv("PLC_USER_PASSWORD")
	if password == "" {
		fmt.Fprintf(os.Stderr, "Password for %s (%d-%d characters): ", *user, auth.MinPasswordLen, auth.MaxPasswordLen)
		line, _ := bufio.NewReader(os.Stdin).ReadString('\n')
		password = strings.TrimRight(line, "\r\n")
	}
	if err := auth.ValidatePassword(password); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}

	cfg := config.Load()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	db, err := store.Open(ctx, cfg.DatabaseURL)
	if err != nil {
		fmt.Fprintln(os.Stderr, "PostgreSQL:", err)
		os.Exit(1)
	}
	defer db.Close()

	name := strings.TrimSpace(*user)
	if _, err := db.CreateUser(ctx, name, auth.StorePassword(password), *role); err != nil {
		var ce *store.ConstraintError
		if errors.As(err, &ce) {
			fmt.Fprintln(os.Stderr, "could not create the user: that username already exists (names are not case-sensitive)")
		} else {
			fmt.Fprintln(os.Stderr, "could not create the user:", err)
		}
		os.Exit(1)
	}
	fmt.Printf("user %q created with role %q\n", name, *role)
}
