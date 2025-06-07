package main

import (
	"fmt"
	"math/rand"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// main is the entry point of the application
func main() {
	// Initialize random number generator for random intervals
	rand.Seed(time.Now().UnixNano())

	config, err := loadConfig("config.json")
	if err != nil {
		fmt.Printf("❌ CRITICAL: Failed to load config: %v\n", err)
		os.Exit(1)
	}

	if strings.Contains(config.Username, "CHANGE_THIS") ||
		strings.Contains(config.Password, "CHANGE_THIS") ||
		(len(config.Messages) > 0 && strings.Contains(config.Messages[0], "ADD_YOUR_MESSAGES")) {
		fmt.Println("🚨 CONFIGURATION REQUIRED:")
		fmt.Println("Please edit 'config.json' and update:")
		fmt.Println("- username: your actual username")
		fmt.Println("- password: your actual password")
		fmt.Println("- messages: your custom messages array")
		fmt.Println("- loginAction: configure if you want login actions")
		fmt.Println("Then run the bot again!")
		os.Exit(1)
	}

	// Handle command line arguments
	if len(os.Args) > 1 {
		config.Username = os.Args[1]
	}
	if len(os.Args) > 2 {
		config.Password = os.Args[2]
	}
	if len(os.Args) > 3 {
		if interval, errAtoi := strconv.Atoi(os.Args[3]); errAtoi == nil && interval > 0 {
			// Convert old-style command line argument to new format
			config.IntervalSeconds = &IntervalConfig{
				From: interval,
				To:   interval,
			}
		}
	}

	// Create TUI and bot
	tui := NewTUI()
	botInstance := NewChatBot(config, tui)

	// Setup signal handling
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, os.Interrupt, syscall.SIGTERM)
	go func() {
		sig := <-sigChan
		botInstance.log("🛑 Received signal: %v. Initiating graceful shutdown...", sig)
		botInstance.Stop()
		tui.Stop() // This will close log files
		fmt.Println("\n👋 Goodbye! Logs saved to logs/ directory.")
		os.Exit(0)
	}()

	// Start the bot in a goroutine
	go func() {
		// Add a small delay to ensure TUI is ready
		time.Sleep(500 * time.Millisecond)
		if errStart := botInstance.Start(); errStart != nil {
			tui.AddLogMessage(fmt.Sprintf("❌ CRITICAL: Failed to start bot: %v", errStart))
		}
	}()

	// Set focus to input field and ensure it's active
	tui.app.SetFocus(tui.inputField)

	// Run the TUI (this blocks until the application exits)
	if err := tui.Run(); err != nil {
		fmt.Printf("❌ TUI Error: %v\n", err)
		os.Exit(1)
	}
}
