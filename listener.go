package main

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// listenForMessages reads messages from WebSocket and processes them
func (bot *ChatBot) listenForMessages() {
	bot.log("👂 Starting message listener...")

	for {
		// Check if we need to reconnect
		if !bot.isConnectionHealthy() {
			bot.log("🔄 Connection unhealthy, attempting full reconnection...")

			if err := bot.fullReconnect(); err != nil {
				bot.log("❌ Full reconnection failed: %v. Will retry...", err)
				continue
			}

			bot.log("✅ Reconnection successful, resuming message listening...")
			continue
		}

		// Set read deadline to detect dead connections
		bot.wsConn.SetReadDeadline(time.Now().Add(90 * time.Second))
		_, message, err := bot.wsConn.ReadMessage()

		if err != nil {
			bot.log("❌ Error reading message: %v", err)

			// Mark connection as unhealthy
			bot.setConnectionStatus(false)

			// Close the connection
			if bot.wsConn != nil {
				bot.wsConn.Close()
				bot.wsConn = nil
			}

			bot.log("🔄 Connection lost, will attempt reconnection...")
			continue
		}

		// Reset read deadline on successful read
		bot.wsConn.SetReadDeadline(time.Time{})
		bot.log("📥 Raw message received: %s", string(message))

		var messageData map[string]interface{}
		if errJson := json.Unmarshal(message, &messageData); errJson == nil {
			bot.processMessage(messageData)
		} else {
			bot.log("⚠️ Could not parse message as JSON: %v. Message: %s", errJson, string(message))
		}
	}
}

// processMessage handles a parsed WebSocket message
func (bot *ChatBot) processMessage(messageData map[string]interface{}) {
	// Handle room changes and initial room data
	if roomInfo, ok := messageData["room"].(map[string]interface{}); ok {
		bot.handleRoomInfo(roomInfo, messageData)
	}

	// Handle client updates (user joins and position updates)
	if clientsData, ok := messageData["clients"].(map[string]interface{}); ok {
		bot.handleClientUpdates(clientsData, messageData)
	}

	// Handle item pickups (items being removed from the game)
	if itemsData, ok := messageData["items"].(map[string]interface{}); ok {
		bot.handleItemUpdates(itemsData)
	}

	// Handle system events
	if player, hasPlayer := messageData["player"].(map[string]interface{}); hasPlayer {
		bot.handlePlayerEvents(player)
	}

	// Handle chat messages - display in chat pane (legacy format, keeping for compatibility)
	bot.handleLegacyChatMessages(messageData)
}

// handleRoomInfo processes room information updates
func (bot *ChatBot) handleRoomInfo(roomInfo map[string]interface{}, messageData map[string]interface{}) {
	if idVal, idOk := roomInfo["id"]; idOk {
		newRoomID := fmt.Sprintf("%v", idVal)
		if nameVal, nameOk := roomInfo["name"].(string); nameOk {
			if bot.currentRoom != newRoomID {
				bot.log("🚪 Room changed by server: %s -> %s (%s)", bot.currentRoom, newRoomID, nameVal)
				bot.currentRoom = newRoomID
				bot.currentRoomName = nameVal
				bot.tui.SetRoom(newRoomID, nameVal)
				bot.updateFullStatus()
			}
		}
	}

	// Parse room fields for movement
	if fields, fieldsOk := roomInfo["fields"].([]interface{}); fieldsOk {
		bot.parseRoomFields(fields)
	}

	// Parse initial client list when joining room
	if clientsData, clientsOk := messageData["clients"].(map[string]interface{}); clientsOk {
		if updates, updatesOk := clientsData["updates"].([]interface{}); updatesOk {
			var users []string
			for _, update := range updates {
				if client, clientOk := update.(map[string]interface{}); clientOk {
					var clientID int
					var username string

					if id, idOk := client["id"].(float64); idOk {
						clientID = int(id)
					}
					if name, nameOk := client["username"].(string); nameOk {
						username = name
						users = append(users, username)

						// Store client ID mapping
						if clientID != 0 {
							bot.setClientUsername(clientID, username)
						}

						// Track initial positions
						if x, xOk := client["x"].(float64); xOk {
							if y, yOk := client["y"].(float64); yOk {
								bot.updateClientPosition(clientID, int(x), int(y))
							}
						}
					}
				}
			}
			bot.tui.SetRoomUsers(users)
			bot.log("👥 Found %d users in room", len(users))
		}
	}
}

// handleClientUpdates processes client-related updates
func (bot *ChatBot) handleClientUpdates(clientsData map[string]interface{}, messageData map[string]interface{}) {
	// Handle user updates
	if updates, updatesOk := clientsData["updates"].([]interface{}); updatesOk {
		for _, update := range updates {
			if client, clientOk := update.(map[string]interface{}); clientOk {
				bot.processClientUpdate(client, messageData)
			}
		}
	}

	// Handle user leaves/disconnects (removes)
	if removes, removesOk := clientsData["removes"].([]interface{}); removesOk {
		bot.handleClientRemovals(removes, messageData)
	}
}

// processClientUpdate handles a single client update
func (bot *ChatBot) processClientUpdate(client map[string]interface{}, messageData map[string]interface{}) {
	var clientID int
	var username string
	var userX, userY int
	hasPosition := false

	// Extract client ID
	if id, idOk := client["id"].(float64); idOk {
		clientID = int(id)
	}

	// Extract username if present
	if name, nameOk := client["username"].(string); nameOk {
		username = name
		// Store the client ID to username mapping
		if clientID != 0 {
			bot.setClientUsername(clientID, username)
		}
	}

	// Extract position if present
	if x, xOk := client["x"].(float64); xOk {
		userX = int(x)
		hasPosition = true
		if y, yOk := client["y"].(float64); yOk {
			userY = int(y)
		}
	}

	// Update position tracking by client ID
	if clientID != 0 && hasPosition {
		bot.updateClientPosition(clientID, userX, userY)
	}

	// Handle new user joins (when we have username but no room data)
	if username != "" && clientID != 0 {
		if _, hasRoom := messageData["room"]; !hasRoom {
			bot.tui.AddUser(username)
			bot.log("👤 User joined: %s (ClientID:%d)", username, clientID)
		}
	}

	// Handle chat messages
	if events, eventsOk := client["events"].([]interface{}); eventsOk {
		bot.handleClientEvents(events)
	}

	// Handle item removal (items being dropped) - now using client ID
	if items, itemsOk := client["items"].(map[string]interface{}); itemsOk {
		bot.handleClientItemRemovals(items, clientID, messageData)
	}
}

// handleClientEvents processes events from a client
func (bot *ChatBot) handleClientEvents(events []interface{}) {
	for _, event := range events {
		if eventData, eventOk := event.(map[string]interface{}); eventOk {
			if eventType, typeOk := eventData["type"].(string); typeOk && eventType == "chat" {
				if data, dataOk := eventData["data"].(map[string]interface{}); dataOk {
					if chatUsername, usernameOk := data["username"].(string); usernameOk {
						if message, messageOk := data["message"].(string); messageOk {
							bot.log("💬 Chat [%s]: %s", chatUsername, message)
							if chatUsername != bot.username {
								bot.tui.AddChatMessage("", chatUsername, message)
							}
						}
					}
				}
			}
		}
	}
}

// handleClientItemRemovals processes item removals from clients
func (bot *ChatBot) handleClientItemRemovals(items map[string]interface{}, clientID int, messageData map[string]interface{}) {
	if removes, removesOk := items["removes"].([]interface{}); removesOk {
		for _, removeItem := range removes {
			if itemID, itemIDOk := removeItem.(float64); itemIDOk {
				// Check if there's a corresponding item update (item being dropped)
				if itemsUpdates, hasItemsUpdates := messageData["items"].(map[string]interface{}); hasItemsUpdates {
					if updatesArray, updatesOk := itemsUpdates["updates"].([]interface{}); updatesOk {
						for _, itemUpdate := range updatesArray {
							if item, itemOk := itemUpdate.(map[string]interface{}); itemOk {
								if id, idOk := item["id"].(float64); idOk && int(id) == int(itemID) {
									// This is an item being dropped
									itemName := "Unknown Item"
									if name, nameOk := item["name"].(string); nameOk {
										itemName = name
									}
									itemX, itemY := 0, 0
									if x, xOk := item["x"].(float64); xOk {
										itemX = int(x)
									}
									if y, yOk := item["y"].(float64); yOk {
										itemY = int(y)
									}

									bot.handleItemDropped(int(itemID), itemName, itemX, itemY, clientID)
								}
							}
						}
					}
				}
			}
		}
	}
}

// handleClientRemovals processes client disconnections
func (bot *ChatBot) handleClientRemovals(removes []interface{}, messageData map[string]interface{}) {
	for _, removeData := range removes {
		if clientID, clientIDOk := removeData.(float64); clientIDOk {
			// Remove from client mapping
			bot.removeClient(int(clientID))
		}
	}

	// Parse system messages to get usernames for removes
	if player, playerOk := messageData["player"].(map[string]interface{}); playerOk {
		if events, eventsOk := player["events"].([]interface{}); eventsOk {
			for _, event := range events {
				if eventData, eventOk := event.(map[string]interface{}); eventOk {
					if eventType, typeOk := eventData["type"].(string); typeOk && eventType == "system" {
						if data, dataOk := eventData["data"].(map[string]interface{}); dataOk {
							if message, msgOk := data["message"].(string); msgOk {
								// Parse system messages like "XXX gik til Receptionen" or "XXX mistede forbindelsen"
								if strings.Contains(message, "gik til") || strings.Contains(message, "mistede forbindelsen") {
									parts := strings.Fields(message)
									if len(parts) > 0 {
										username := parts[0]
										bot.tui.RemoveUser(username)
										bot.tui.AddChatMessage("", "", message)
										bot.log("👤 User left/disconnected: %s", username)
									}
								}
							}
						}
					}
				}
			}
		}
	}
}

// handleItemUpdates processes item-related updates
func (bot *ChatBot) handleItemUpdates(itemsData map[string]interface{}) {
	if removes, removesOk := itemsData["removes"].([]interface{}); removesOk {
		for _, removeItem := range removes {
			if itemID, itemIDOk := removeItem.(float64); itemIDOk {
				// This is an item being picked up by someone
				bot.handleItemPickedUp(int(itemID))
			}
		}
	}
}

// handlePlayerEvents processes player-specific events
func (bot *ChatBot) handlePlayerEvents(player map[string]interface{}) {
	// Handle onlineTime updates
	if onlineTime, hasOnlineTime := player["onlineTime"].(map[string]interface{}); hasOnlineTime {
		if hours, hasHours := onlineTime["hours"]; hasHours {
			if minutes, hasMinutes := onlineTime["minutes"]; hasMinutes {
				if h, hOk := hours.(float64); hOk {
					if m, mOk := minutes.(float64); mOk {
						bot.tui.UpdateOnlineTime(int(h), int(m))
						bot.updateFullStatus()
						bot.log("⏰ Online time updated: %dh %dm", int(h), int(m))
					}
				}
			}
		}
	}

	// Handle newHour events
	if newHour, hasNewHour := player["newHour"].(bool); hasNewHour && newHour {
		bot.log("🕐 'newHour' event detected from server - sending acknowledgment...")
		go func() {
			if errAck := bot.sendAcknowledgment("newHour"); errAck != nil {
				bot.log("❌ Failed to acknowledge 'newHour' event: %v", errAck)
			}
		}()
	}

	// Handle system events like user arrivals
	if events, eventsOk := player["events"].([]interface{}); eventsOk {
		for _, event := range events {
			if eventData, eventOk := event.(map[string]interface{}); eventOk {
				if eventType, typeOk := eventData["type"].(string); typeOk && eventType == "system" {
					if data, dataOk := eventData["data"].(map[string]interface{}); dataOk {
						if message, msgOk := data["message"].(string); msgOk {
							// Handle user arrival messages
							if strings.Contains(message, "ankom til rummet") {
								parts := strings.Fields(message)
								if len(parts) > 0 {
									username := parts[0]
									bot.tui.AddUser(username)
									bot.tui.AddChatMessage("", "", message)
									bot.log("👤 User arrived: %s", username)
								}
							} else if strings.Contains(message, "Du er ankommet til") {
								// Handle own arrival message
								bot.tui.AddChatMessage("", "", message)
							}
						}
					}
				}
			}
		}
	}
}

// handleLegacyChatMessages handles legacy chat message formats
func (bot *ChatBot) handleLegacyChatMessages(messageData map[string]interface{}) {
	if msgType, ok := messageData["type"].(string); ok && (msgType == "chat" || msgType == "message") {
		if data, dataOk := messageData["data"].(map[string]interface{}); dataOk {
			username, _ := data["username"].(string)
			chatMsg, _ := data["message"].(string)
			if username != "" && chatMsg != "" {
				bot.log("💬 Chat [%s]: %s", username, chatMsg)
				// Don't display your own messages again (they're already shown when sent)
				if username != bot.username {
					bot.tui.AddChatMessage("", username, chatMsg)
				}
			}
		}
	}
	if user, userOk := messageData["user"].(string); userOk {
		if text, textOk := messageData["text"].(string); textOk {
			bot.log("💬 Chat [%s]: %s", user, text)
			// Don't display your own messages again (they're already shown when sent)
			if user != bot.username {
				bot.tui.AddChatMessage("", user, text)
			}
		}
	}
}
