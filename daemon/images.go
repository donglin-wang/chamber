package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	daemonconfig "github.com/donglin-wang/chamber/daemon/config"
	"github.com/donglin-wang/chamber/daemon/metadata"
	chimage "github.com/donglin-wang/chamber/internal/image"
	"github.com/google/uuid"
)

type pullImageRequest struct {
	Reference string `json:"reference"`
}

type pullImageResponse struct {
	OperationID string    `json:"operation_id"`
	Reference   string    `json:"reference"`
	Digest      string    `json:"digest"`
	PulledAt    time.Time `json:"pulled_at"`
}

func registerImageRoutes(mux *http.ServeMux, cfg daemonconfig.Config, store metadata.Store, puller chimage.Puller) {
	mux.HandleFunc("POST /v1/images/pull", func(w http.ResponseWriter, r *http.Request) {
		var request pullImageRequest
		if err := decodeJSON(w, r, &request); err != nil {
			writeError(w, http.StatusBadRequest, "invalid_request", "request body must be a JSON object with a reference")
			return
		}

		if strings.TrimSpace(request.Reference) == "" {
			writeError(w, http.StatusBadRequest, "invalid_request", "reference is required")
			return
		}

		result, err := pullImage(r.Context(), cfg, store, puller, strings.TrimSpace(request.Reference))
		if err != nil {
			writeDaemonError(w, err)
			return
		}

		writeOperationJSON(w, http.StatusOK, result.operation.ID, pullImageResponse{
			OperationID: result.operation.ID,
			Reference:   result.image.Reference,
			Digest:      result.image.Digest,
			PulledAt:    result.image.PulledAt,
		})
	})
}

type pullImageResult struct {
	operation metadata.Operation
	image     metadata.Image
}

func pullImage(ctx context.Context, cfg daemonconfig.Config, store metadata.Store, puller chimage.Puller, reference string) (pullImageResult, error) {
	if store == nil {
		return pullImageResult{}, fmt.Errorf("metadata store is required")
	}
	if puller == nil {
		return pullImageResult{}, fmt.Errorf("image puller is required")
	}

	startedAt := time.Now().UTC()
	operationUUID, err := uuid.NewV7()
	if err != nil {
		return pullImageResult{}, fmt.Errorf("generate pull operation id: %w", err)
	}
	operationID := operationUUID.String()
	operation := metadata.Operation{
		ID:         operationID,
		Kind:       metadata.PullOperation,
		State:      metadata.OperationRunning,
		ResourceID: reference,
		StartedAt:  startedAt,
		UpdatedAt:  startedAt,
	}
	if err := store.CreateOperation(ctx, operation); err != nil {
		return pullImageResult{}, fmt.Errorf("create pull operation: %w", err)
	}

	destination, err := chimage.Destination(cfg.Image.Root, reference)
	if err != nil {
		_, transitionErr := store.FailOperation(ctx, operationID, metadata.ErrInvalidRequest)
		failErr := operationError(operationID, metadata.ErrInvalidRequest, errors.Join(err, transitionErr))
		return pullImageResult{operation: operation}, failErr
	}

	existing, err := store.GetImage(ctx, reference)
	if err == nil && chimage.LayoutExists(existing.LayoutPath) {
		completed, err := store.SucceedOperation(ctx, operationID)
		if err != nil {
			return pullImageResult{operation: operation, image: existing}, operationError(operationID, metadata.ErrMetadataFailed, err)
		}
		return pullImageResult{
			operation: completed,
			image:     existing,
		}, nil
	}
	if err != nil && !errors.Is(err, metadata.ErrNotFound) {
		_, transitionErr := store.FailOperation(ctx, operationID, metadata.ErrMetadataFailed)
		failErr := operationError(operationID, metadata.ErrMetadataFailed, errors.Join(err, transitionErr))
		return pullImageResult{operation: operation}, failErr
	}

	pulled, err := puller.Pull(ctx, chimage.PullRequest{
		Reference:   reference,
		Destination: destination,
	})
	if err != nil {
		_, transitionErr := store.FailOperation(ctx, operationID, metadata.ErrPullFailed)
		failErr := operationError(operationID, metadata.ErrPullFailed, errors.Join(err, transitionErr))
		return pullImageResult{operation: operation}, failErr
	}

	pulledAt := pulled.PulledAt
	if pulledAt.IsZero() {
		pulledAt = time.Now().UTC()
	}
	layoutPath := pulled.LayoutPath
	if layoutPath == "" {
		layoutPath = destination
	}
	image := metadata.Image{
		Reference:  reference,
		Digest:     pulled.Digest,
		LayoutPath: layoutPath,
		PulledAt:   pulledAt,
		LastUsedAt: pulledAt,
	}
	if err := store.PutImage(ctx, image); err != nil {
		_, transitionErr := store.FailOperation(ctx, operationID, metadata.ErrMetadataFailed)
		failErr := operationError(operationID, metadata.ErrMetadataFailed, errors.Join(err, transitionErr))
		return pullImageResult{operation: operation, image: image}, failErr
	}

	completed, err := store.SucceedOperation(ctx, operationID)
	if err != nil {
		return pullImageResult{operation: operation, image: image}, operationError(operationID, metadata.ErrMetadataFailed, err)
	}

	return pullImageResult{
		operation: completed,
		image:     image,
	}, nil
}
