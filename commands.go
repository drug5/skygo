package main

import (
	"fmt"
	"os"
	"strings"
	"time"
)

// executeLoginActions performs the configured login actions after successful connection
func (bot *ChatBot) executeLoginActions() {
	// Check if login actions are enabled and not already executed
	bot.loginActionMutex.Lock()
	if bot.loginAction == nil || !bot.loginAction.LoginActionEnabled || bot.loginActionDone {
		bot.loginActionMutex.Unlock()
		return
	}
	
	// Mark as in progress to prevent multiple executions
	bot.loginActionDone = true
	bot.loginActionMutex.Unlock()

	if len(bot.loginAction.Commands) == 0 {
		bot.log("🤖 Login actions enabled but no commands configured")
		return
	}

	bot.log("🤖 Login actions enabled - waiting %d seconds before execution...", bot.loginAction.WaitSeconds)
	bot.tui.AddChatMessage("", "", fmt.Sprintf("*** Login actions will execute in %d seconds... ***", bot.loginAction.WaitSeconds))

	// Wait the specified time before starting
	time.Sleep(time.Duration(bot.loginAction.WaitSeconds) * time.Second)

	bot.log("🤖 Starting execution of %d login action commands", len(bot.loginAction.Commands))
	bot.tui.AddChatMessage("", "", "*** Executing login actions... ***")

	for i, command := range bot.loginAction.Commands {
		// Skip example commands
		if strings.HasPrefix(command, "EXAMPLE:") {
			bot.log("🤖 Skipping example command: %s", command)
			continue
		}

		bot.log("🤖 Executing login action command %d/%d: %s", i+1, len(bot.loginAction.Commands), command)
		
		// Execute the command using the same logic as manual input
		go func(cmd string) {
			bot.executeCommand(cmd)
		}(command)

		// Wait between commands (except after the last one)
		if i < len(bot.loginAction.Commands)-1 {
			time.Sleep(time.Duration(bot.loginAction.TimeBetweenCommands) * time.Second)
		}
	}

	bot.log("🤖 Login actions completed")
	bot.tui.AddChatMessage("", "", "*** Login actions completed ***")
}

// executeCommand executes a single command (used by both manual input and login actions)
func (bot *ChatBot) executeCommand(input string) {
	if input == "" {
		return
	}

	// Handle commands
	if strings.HasPrefix(input, "/") {
		command := strings.Fields(input)[0]

		switch command {
		case "/quit", "/exit", "/q":
			bot.log("⌨️ Quit command received. Shutting down...")
			bot.Stop()
			bot.tui.app.Stop()
			os.Exit(0)
		case "/help", "/h":
			bot.tui.AddChatMessage("", "", "Available Commands:")
			bot.tui.AddChatMessage("", "", "/quit, /exit, /q - Exit the bot")
			bot.tui.AddChatMessage("", "", "/help, /h - Show this help")
			bot.tui.AddChatMessage("", "", "/status - Show bot status")
			bot.tui.AddChatMessage("", "", "/reconnect - Force reconnection")
			bot.tui.AddChatMessage("", "", "/clear - Clear chat history")
			bot.tui.AddChatMessage("", "", "/logs - Show log file information")
			bot.tui.AddChatMessage("", "", "/autopick - Toggle autopick on/off")
			bot.tui.AddChatMessage("", "", "/move - Move to random open field")
			bot.tui.AddChatMessage("", "", "/loginaction - Show login action status")
		case "/status":
			wsStatus := "Disconnected"
			if bot.isConnectionHealthy() {
				wsStatus = "Connected"
			}
			autopickStatus := "DISABLED"
			if bot.isAutopickEnabled() {
				autopickStatus = "ENABLED"
			}
			hours, minutes := bot.tui.GetOnlineTime()
			bot.tui.AddChatMessage("", "", fmt.Sprintf("Username: %s", bot.username))
			bot.tui.AddChatMessage("", "", fmt.Sprintf("Current Room: %s (%s)", bot.currentRoomName, bot.currentRoom))
			bot.tui.AddChatMessage("", "", fmt.Sprintf("WebSocket: %s", wsStatus))
			bot.tui.AddChatMessage("", "", fmt.Sprintf("Online Time: %dh %dm", hours, minutes))
			bot.tui.AddChatMessage("", "", fmt.Sprintf("Message Interval: %s", bot.getIntervalString()))
			bot.tui.AddChatMessage("", "", fmt.Sprintf("Autopick: %s", autopickStatus))
			bot.tui.AddChatMessage("", "", fmt.Sprintf("Reconnect Count: %d", bot.reconnectCount))
			if !bot.lastReconnect.IsZero() {
				bot.tui.AddChatMessage("", "", fmt.Sprintf("Last Reconnect: %s", bot.lastReconnect.Format("15:04:05")))
			}

			// Show tracked items if autopick is enabled
			if bot.isAutopickEnabled() {
				bot.positionMutex.RLock()
				itemCount := len(bot.droppedItems)
				userCount := len(bot.clientPositions)
				bot.positionMutex.RUnlock()
				bot.tui.AddChatMessage("", "", fmt.Sprintf("Tracked Items: %d", itemCount))
				bot.tui.AddChatMessage("", "", fmt.Sprintf("Tracked Clients: %d", userCount))
			}
		case "/reconnect":
			bot.tui.AddChatMessage("", "", "*** Forcing reconnection... ***")
			go func() {
				bot.setConnectionStatus(false)
				if bot.wsConn != nil {
					bot.wsConn.Close()
					bot.wsConn = nil
				}
				if err := bot.fullReconnect(); err != nil {
					bot.log("❌ Manual reconnection failed: %v", err)
					bot.tui.AddChatMessage("", "", "*** Reconnection failed ***")
				}
			}()
		case "/move":
			availablePositions := bot.getAvailablePositions()
			if len(availablePositions) == 0 {
				bot.tui.AddChatMessage("", "", "❌ No available positions to move to")
			} else {
				if err := bot.moveToRandomPosition(); err != nil {
					bot.tui.AddChatMessage("", "", fmt.Sprintf("❌ Failed to move: %v", err))
				} else {
					bot.tui.AddChatMessage("", "", fmt.Sprintf("🎲 Moving to random position (%d available)", len(availablePositions)))
				}
			}
		case "/autopick":
			currentStatus := bot.isAutopickEnabled()
			bot.setAutopickEnabled(!currentStatus)
			newStatus := "ENABLED"
			if !bot.isAutopickEnabled() {
				newStatus = "DISABLED"
			}
			bot.tui.AddChatMessage("", "", fmt.Sprintf("🤖 Autopick %s", newStatus))
		case "/loginaction":
			bot.loginActionMutex.Lock()
			if bot.loginAction == nil {
				bot.tui.AddChatMessage("", "", "🤖 Login actions: Not configured")
			} else if !bot.loginAction.LoginActionEnabled {
				bot.tui.AddChatMessage("", "", "🤖 Login actions: DISABLED")
			} else {
				status := "PENDING"
				if bot.loginActionDone {
					status = "COMPLETED"
				}
				bot.tui.AddChatMessage("", "", fmt.Sprintf("🤖 Login actions: ENABLED (%s)", status))
				bot.tui.AddChatMessage("", "", fmt.Sprintf("  Wait time: %d seconds", bot.loginAction.WaitSeconds))
				bot.tui.AddChatMessage("", "", fmt.Sprintf("  Command delay: %d seconds", bot.loginAction.TimeBetweenCommands))
				bot.tui.AddChatMessage("", "", fmt.Sprintf("  Commands: %d configured", len(bot.loginAction.Commands)))
			}
			bot.loginActionMutex.Unlock()
		case "/logs":
			currentDate := time.Now().Format("2006-01-02")
			chatLogPath := fmt.Sprintf("logs/%s_chat_log.txt", currentDate)
			rawLogPath := fmt.Sprintf("logs/%s_raw_log.txt", currentDate)

			bot.tui.AddChatMessage("", "", "📁 Log File Information:")
			bot.tui.AddChatMessage("", "", fmt.Sprintf("Chat Log: %s", chatLogPath))
			bot.tui.AddChatMessage("", "", fmt.Sprintf("System Log: %s", rawLogPath))
			bot.tui.AddChatMessage("", "", "All logs are saved with daily rotation")
			bot.tui.AddChatMessage("", "", "Format: YYYY-MM-DD_[chat_log|raw_log].txt")
		case "/clear":
			bot.tui.app.QueueUpdateDraw(func() {
				bot.tui.chatView.Clear()
			})
		default:
			// Send command as message
			bot.log("⌨️ Sending command as message: '%s'", input)
			if err := bot.sendConsoleMessage(input); err != nil {
				bot.log("❌ Failed to send console message: %v", err)
			}
		}
	} else {
		// Send regular message
		bot.log("⌨️ Sending message: '%s'", input)
		if err := bot.sendConsoleMessage(input); err != nil {
			bot.log("❌ Failed to send console message: %v", err)
		}
	}
}
