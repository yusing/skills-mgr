package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
)

type registrySearchSkill struct {
	ID    string
	Name  string
	Label string
}

type registrySearchProvider interface {
	search(context.Context, string) ([]registrySearchSkill, error)
}

func normalizedRegistryQuery(query string) string {
	return strings.ToLower(strings.Join(strings.Fields(query), " "))
}

func fetchRegistryJSON(
	ctx context.Context,
	client *http.Client,
	endpoint string,
	authorization string,
	result any,
) error {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return err
	}
	request.Header.Set("Accept", "application/json")
	if authorization != "" {
		request.Header.Set("Authorization", authorization)
	}
	response, err := client.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()

	body, err := io.ReadAll(io.LimitReader(response.Body, remoteResponseLimit+1))
	if err != nil {
		return err
	}
	if len(body) > remoteResponseLimit {
		return fmt.Errorf("response exceeds %d bytes", remoteResponseLimit)
	}
	if response.StatusCode != http.StatusOK {
		if message := registryErrorMessage(body); message != "" {
			return fmt.Errorf("%s: %s", response.Status, message)
		}
		return fmt.Errorf("unexpected HTTP status %s", response.Status)
	}
	if err := json.Unmarshal(body, result); err != nil {
		return fmt.Errorf("decode response: %w", err)
	}
	return nil
}

func registryErrorMessage(body []byte) string {
	var response struct {
		Error   json.RawMessage `json:"error"`
		Message string          `json:"message"`
	}
	if json.Unmarshal(body, &response) != nil {
		return ""
	}
	var nested struct {
		Message string `json:"message"`
	}
	if json.Unmarshal(response.Error, &nested) == nil && nested.Message != "" {
		return nested.Message
	}
	return response.Message
}
