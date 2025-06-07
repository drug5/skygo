package main

import (
	"encoding/json"
	"fmt"
	"os"
)

// loadConfig loads configuration from a JSON file or creates a template
func loadConfig(configPath string) (*Config, error) {
	defaultConfig := &Config{
		Username: "CHANGE_THIS_USERNAME",
		Password: "CHANGE_THIS_PASSWORD",
		IntervalSeconds: &IntervalConfig{
			From: 250,
			To:   350,
		},
		LoginAction: &LoginAction{
			LoginActionEnabled:   false,
			WaitSeconds:         30,
			TimeBetweenCommands: 10,
			Commands: []string{
				"EXAMPLE: /move",
				"EXAMPLE: Hello everyone!",
			},
		},
		Messages: []string{
			"ADD_YOUR_MESSAGES_HERE",
			"EXAMPLE: Hej alle!",
			"EXAMPLE: Hvordan har I det?",
		},
		BaseURL:      "https://www.skyskraber.dk",
		WebSocketURL: "wss://www.skyskraber.dk/ws",
	}

	configFile, err := os.ReadFile(configPath)
	if err != nil {
		if os.IsNotExist(err) {
			configData, errJson := json.MarshalIndent(defaultConfig, "", "  ")
			if errJson != nil {
				return nil, fmt.Errorf("failed to marshal template config: %v", errJson)
			}

			if errWrite := os.WriteFile(configPath, configData, 0644); errWrite != nil {
				return nil, fmt.Errorf("failed to create template config file: %v", errWrite)
			}

			return defaultConfig, nil
		}
		return nil, fmt.Errorf("failed to read config file: %v", err)
	}

	// First, parse into a generic map to handle intervalSeconds flexibility
	var rawConfig map[string]interface{}
	if errJson := json.Unmarshal(configFile, &rawConfig); errJson != nil {
		return nil, fmt.Errorf("failed to parse config file: %v", errJson)
	}

	// Create the config struct
	config := &Config{}

	// Handle intervalSeconds - support both old int format and new object format
	if intervalData, exists := rawConfig["intervalSeconds"]; exists {
		switch v := intervalData.(type) {
		case float64: // JSON numbers are parsed as float64
			// Old format: single integer
			seconds := int(v)
			config.IntervalSeconds = &IntervalConfig{
				From: seconds,
				To:   seconds,
			}
		case map[string]interface{}: // New format: object with from/to
			intervalConfig := &IntervalConfig{}
			if from, ok := v["from"].(float64); ok {
				intervalConfig.From = int(from)
			}
			if to, ok := v["to"].(float64); ok {
				intervalConfig.To = int(to)
			}
			// Validate that from <= to
			if intervalConfig.From > intervalConfig.To {
				return nil, fmt.Errorf("intervalSeconds: 'from' (%d) cannot be greater than 'to' (%d)", intervalConfig.From, intervalConfig.To)
			}
			config.IntervalSeconds = intervalConfig
		default:
			return nil, fmt.Errorf("intervalSeconds must be either a number or an object with 'from' and 'to' fields")
		}
	}

	// Parse the rest of the config normally
	if errJson := json.Unmarshal(configFile, config); errJson != nil {
		return nil, fmt.Errorf("failed to parse config file: %v", errJson)
	}

	// Set defaults for missing fields
	if config.BaseURL == "" {
		config.BaseURL = "https://www.skyskraber.dk"
	}
	if config.WebSocketURL == "" {
		config.WebSocketURL = "wss://www.skyskraber.dk/ws"
	}
	if config.IntervalSeconds == nil {
		config.IntervalSeconds = &IntervalConfig{From: 300, To: 300}
	}
	if config.LoginAction == nil {
		config.LoginAction = &LoginAction{
			LoginActionEnabled:   false,
			WaitSeconds:         30,
			TimeBetweenCommands: 10,
			Commands:            []string{},
		}
	}

	// Validate interval configuration
	if config.IntervalSeconds.From <= 0 || config.IntervalSeconds.To <= 0 {
		return nil, fmt.Errorf("intervalSeconds: both 'from' and 'to' must be positive numbers")
	}
	if config.IntervalSeconds.From > config.IntervalSeconds.To {
		return nil, fmt.Errorf("intervalSeconds: 'from' (%d) cannot be greater than 'to' (%d)", config.IntervalSeconds.From, config.IntervalSeconds.To)
	}

	return config, nil
}
