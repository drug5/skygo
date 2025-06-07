package main

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/gdamore/tcell/v2"
	"github.com/gorilla/websocket"
)

// sendMessageInternal sends a message over WebSocket and optionally resets the periodic timer
func (bot *ChatBot) sendMessageInternal(message string, resetTimer bool) error {
	if !bot.isConnectionHealthy() {
		return fmt.Errorf("WebSocket connection not healthy")
	}

	bot.wsMutex.Lock()
	defer bot.wsMutex.Unlock()

	if bot.wsConn == nil {
		return fmt.Errorf("WebSocket connection not established")
	}

	bot.log("💬 Preparing to send message to room %s: '%s'", bot.currentRoom, message)

	msgPayload := map[string]interface{}{
		"type": "chat",
		"data": map[string]interface{}{
			"message": message,
			"room":    bot.currentRoom,
		},
	}

	msgBytes, err := json.Marshal(msgPayload)
	if err != nil {
		bot.log("❌ Failed to marshal message: %v. Payload: %+v", err, msgPayload)
		return fmt.Errorf("failed to marshal message: %v", err)
	}

	bot.log("📤 Sending message: %s", string(msgBytes))

	// Set a write deadline to prevent hanging
	bot.wsConn.SetWriteDeadline(time.Now().Add(10 * time.Second))
	err = bot.wsConn.WriteMessage(websocket.TextMessage, msgBytes)
	if err != nil {
		bot.log("❌ Failed to send message: %v", err)
		// Mark connection as unhealthy on write failure
		bot.setConnectionStatus(false)
		return fmt.Errorf("failed to send message: %v", err)
	}

	if resetTimer {
		bot.resetMessageTimer()
		bot.log("⏰ Periodic message timer reset after sending manual message.")
	}

	bot.log("✅ Message sent successfully: '%s'", message)
	return nil
}

// sendMessage is a convenience wrapper for sending periodic messages
func (bot *ChatBot) sendMessage(message string) error {
	return bot.sendMessageInternal(message, false)
}

// sendConsoleMessage is for messages typed by user
func (bot *ChatBot) sendConsoleMessage(message string) error {
	err := bot.sendMessageInternal(message, true)
	if err == nil {
		// Show the message in chat pane as well (in goroutine to avoid blocking)
		go func() {
			bot.tui.AddChatMessage("", bot.username, message)
		}()
	}
	return err
}

// resetMessageTimer stops and resets the periodic message timer with a new random interval
func (bot *ChatBot) resetMessageTimer() {
	bot.timerMutex.Lock()
	defer bot.timerMutex.Unlock()

	newInterval := bot.getRandomInterval()
	
	if bot.messageTimer != nil {
		if !bot.messageTimer.Stop() {
			select {
			case <-bot.messageTimer.C:
			default:
			}
		}
		bot.messageTimer.Reset(newInterval)
		bot.log("⏰ Message timer reset to %v", newInterval)
	} else {
		bot.log("⚠️ Tried to reset a nil message timer.")
	}
}

// sendAcknowledgment sends a specific acknowledgment message to the server
func (bot *ChatBot) sendAcknowledgment(messageType string) error {
	if !bot.isConnectionHealthy() {
		return fmt.Errorf("WebSocket connection not healthy")
	}

	bot.wsMutex.Lock()
	defer bot.wsMutex.Unlock()

	if bot.wsConn == nil {
		return fmt.Errorf("WebSocket connection not established")
	}

	bot.log("ℹ️ Preparing to send acknowledgment for server event: '%s'", messageType)

	var ackPayload interface{}

	switch messageType {
	case "newHour":
		ackPayload = map[string]interface{}{
			"type": "hour",
		}
		bot.log("✅ Using specific acknowledgment payload for '%s'", messageType)
	default:
		bot.log("⚠️ No specific acknowledgment logic defined for messageType: '%s'", messageType)
		return fmt.Errorf("no specific acknowledgment handler defined for messageType: %s", messageType)
	}

	msgBytes, err := json.Marshal(ackPayload)
	if err != nil {
		bot.log("❌ Failed to marshal acknowledgment payload for '%s': %v", messageType, err)
		return fmt.Errorf("failed to marshal acknowledgment payload for '%s': %v", messageType, err)
	}

	bot.log("📤 Sending acknowledgment for '%s': %s", messageType, string(msgBytes))
	bot.wsConn.SetWriteDeadline(time.Now().Add(5 * time.Second))
	err = bot.wsConn.WriteMessage(websocket.TextMessage, msgBytes)
	if err != nil {
		bot.log("❌ Failed to send acknowledgment for '%s': %v", messageType, err)
		// Mark connection as unhealthy on write failure
		bot.setConnectionStatus(false)
		return fmt.Errorf("failed to send acknowledgment for '%s': %v", messageType, err)
	}

	bot.log("✅ Acknowledgment for '%s' sent successfully.", messageType)
	return nil
}

// sendPeriodicMessages sends messages from the configured list at random intervals
func (bot *ChatBot) sendPeriodicMessages() {
	if len(bot.messages) == 0 || bot.messages[0] == "ADD_YOUR_MESSAGES_HERE" {
		bot.log("⚠️ No valid periodic messages configured. Periodic sender will not run.")
		return
	}
	bot.log("🕐 Starting periodic message sender (interval: %s)", bot.getIntervalString())

	// Initialize with first random interval
	initialInterval := bot.getRandomInterval()
	bot.log("🎲 First message will be sent in %v", initialInterval)

	bot.timerMutex.Lock()
	if bot.messageTimer == nil {
		bot.messageTimer = time.NewTimer(initialInterval)
	} else {
		bot.messageTimer.Reset(initialInterval)
	}
	bot.timerMutex.Unlock()

	for {
		bot.timerMutex.Lock()
		timerC := bot.messageTimer.C
		bot.timerMutex.Unlock()

		<-timerC

		// Only send if connection is healthy
		if bot.isConnectionHealthy() && len(bot.messages) > 0 {
			message := bot.messages[bot.currentMsgIndex]
			bot.log("⏰ Time to send periodic message [%d/%d]: '%s'", bot.currentMsgIndex+1, len(bot.messages), message)
			if err := bot.sendMessage(message); err != nil {
				bot.log("❌ Failed to send periodic message: %v", err)
				// Don't advance message index on failure
			} else {
				bot.log("✅ Periodic message sent successfully: '%s'", message)
				// Show the message in chat as well
				bot.tui.AddChatMessage("", bot.username, message)
				bot.currentMsgIndex = (bot.currentMsgIndex + 1) % len(bot.messages)
			}
		} else if !bot.isConnectionHealthy() {
			bot.log("⚠️ Skipping periodic message - connection not healthy")
		}

		// Reset timer with new random interval
		newInterval := bot.getRandomInterval()
		bot.log("🎲 Next message will be sent in %v", newInterval)
		
		bot.timerMutex.Lock()
		if bot.messageTimer != nil {
			bot.messageTimer.Reset(newInterval)
		}
		bot.timerMutex.Unlock()
	}
}

// setupInputHandler configures the input field for commands and messages
func (bot *ChatBot) setupInputHandler() {
	bot.tui.inputField.SetDoneFunc(func(key tcell.Key) {
		if key == tcell.KeyEnter {
			input := strings.TrimSpace(bot.tui.inputField.GetText())
			if input == "" {
				return
			}

			// Clear input field immediately for better UX
			bot.tui.inputField.SetText("")

			// Handle commands and messages in goroutine to not block TUI
			go func(message string) {
				bot.executeCommand(message)
			}(input)
		}
	})

	// Set input field as focusable and ensure it gets focus
	bot.tui.app.SetFocus(bot.tui.inputField)
}
