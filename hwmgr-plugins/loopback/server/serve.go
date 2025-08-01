/*
SPDX-FileCopyrightText: Red Hat

SPDX-License-Identifier: Apache-2.0
*/

package server

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/openshift-kni/oran-o2ims/hwmgr-plugins/api"
	"github.com/openshift-kni/oran-o2ims/hwmgr-plugins/api/server/inventory"
	"github.com/openshift-kni/oran-o2ims/hwmgr-plugins/api/server/provisioning"
	"github.com/openshift-kni/oran-o2ims/internal/constants"
	ctlrutils "github.com/openshift-kni/oran-o2ims/internal/controllers/utils"
	common "github.com/openshift-kni/oran-o2ims/internal/service/common/api"
	"github.com/openshift-kni/oran-o2ims/internal/service/common/api/middleware"
	"github.com/openshift-kni/oran-o2ims/internal/service/common/auth"
	svcutils "github.com/openshift-kni/oran-o2ims/internal/service/common/utils"
)

// Loopback HardwarePlugin Server config values
const (
	readTimeout  = 5 * time.Second
	writeTimeout = 10 * time.Second
	idleTimeout  = 120 * time.Second
)

// Serve starts the Loopback HardwarePlugin API server and blocks until it terminates or context is canceled.
func Serve(ctx context.Context, config svcutils.CommonServerConfig, hubClient client.Client) error {
	slog.Info("Initializing the Loopback HardwarePlugin server")

	// Retrieve the OpenAPI spec file
	provisioningAPIswagger, err := provisioning.GetSwagger()
	if err != nil {
		return fmt.Errorf("failed to get swagger: %w", err)
	}

	inventoryAPIswagger, err := inventory.GetSwagger()
	if err != nil {
		return fmt.Errorf("failed to get inventory swagger: %w", err)
	}

	// Channel for shutdown signals
	shutdown := make(chan os.Signal, 1)
	signal.Notify(shutdown, os.Interrupt, syscall.SIGTERM)

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	go func() {
		sig := <-shutdown
		slog.InfoContext(ctx, "Shutdown signal received", slog.String("signal", sig.String()))
		cancel()
	}()

	// Init loopbackProvisioningServer
	loopbackProvisioningServer, err := NewLoopbackPluginServer(
		config,
		hubClient,
		slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{
			AddSource: true,
			Level:     slog.LevelDebug,
		})))
	if err != nil {
		return fmt.Errorf("failed to build Loopback HardwarePlugin provisioning server: %w", err)
	}

	provisioningServerStrictHandler := provisioning.NewStrictHandlerWithOptions(loopbackProvisioningServer, nil,
		provisioning.StrictHTTPServerOptions{
			RequestErrorHandlerFunc:  api.GetRequestErrorFunc(),
			ResponseErrorHandlerFunc: api.GetResponseErrorFunc(),
		},
	)

	// Init loopbackInventoryServer
	loopbackInventoryServer, err := NewLoopbackPluginInventoryServer(
		hubClient,
		slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{
			AddSource: true,
			Level:     slog.LevelDebug,
		})))
	if err != nil {
		return fmt.Errorf("failed to build Loopback HardwarePlugin inventory server: %w", err)
	}

	// Create strict handler for inventory server
	inventoryStrictHandler := inventory.NewStrictHandlerWithOptions(loopbackInventoryServer, nil,
		inventory.StrictHTTPServerOptions{
			RequestErrorHandlerFunc:  api.GetRequestErrorFunc(),
			ResponseErrorHandlerFunc: api.GetResponseErrorFunc(),
		},
	)

	baseRouter := http.NewServeMux()
	// Register a default handler that replies with 404 so that we can override the response format
	baseRouter.HandleFunc("/", api.GetNotFoundFunc())

	// Create authn/authz middleware
	authn, err := auth.GetAuthenticator(ctx, &config)
	if err != nil {
		return fmt.Errorf("error setting up Loopback HardwarePlugin authenticator middleware: %w", err)
	}

	authz, err := auth.GetAuthorizer()
	if err != nil {
		return fmt.Errorf("error setting up Loopback HardwarePlugin authorizer middleware: %w", err)
	}

	// Create subrouters for provisioning and inventory
	provisioningRouter := http.NewServeMux()
	inventoryRouter := http.NewServeMux()

	// Register handlers with subrouters
	provisioning.HandlerWithOptions(provisioningServerStrictHandler, provisioning.StdHTTPServerOptions{
		BaseRouter: provisioningRouter,
		Middlewares: []provisioning.MiddlewareFunc{
			api.GetOpenAPIValidationFunc(provisioningAPIswagger),
			authz,
			authn,
			api.GetLogDurationFunc(),
		},
		ErrorHandlerFunc: api.GetRequestErrorFunc(),
	})
	inventory.HandlerWithOptions(inventoryStrictHandler, inventory.StdHTTPServerOptions{
		BaseRouter: inventoryRouter,
		Middlewares: []inventory.MiddlewareFunc{
			api.GetOpenAPIValidationFunc(inventoryAPIswagger),
			authz,
			authn,
			api.GetLogDurationFunc(),
		},
		ErrorHandlerFunc: api.GetRequestErrorFunc(),
	})

	// Mount subrouters to base router with path prefixes
	baseRouter.Handle(constants.HardwareManagerProvisioningAPIPath+"/", provisioningRouter)
	baseRouter.Handle(constants.HardwareManagerInventoryAPIPath+"/", inventoryRouter)

	// Apply global middleware chain
	handler := middleware.ChainHandlers(
		baseRouter,
		middleware.ErrorJsonifier(),
		middleware.TrailingSlashStripper(),
	)

	serverTLSConfig, err := ctlrutils.GetServerTLSConfig(ctx, config.TLS.CertFile, config.TLS.KeyFile)
	if err != nil {
		return fmt.Errorf("failed to get Loopback HardwarePlugin server TLS config: %w", err)
	}

	srv := &http.Server{
		Handler:      handler,
		Addr:         config.Listener.Address,
		TLSConfig:    serverTLSConfig,
		ReadTimeout:  readTimeout,
		WriteTimeout: writeTimeout,
		IdleTimeout:  idleTimeout,
		ErrorLog:     slog.NewLogLogger(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{AddSource: true}), slog.LevelError),
	}

	// Start server
	serverErrors := make(chan error, 1)
	go func() {
		slog.Info(fmt.Sprintf("Listening on %s", srv.Addr))
		// Cert/Key files aren't needed here since they've been added to the tls.Config above.
		if err := srv.ListenAndServeTLS("", ""); err != nil && !errors.Is(err, http.ErrServerClosed) {
			serverErrors <- err
		}
	}()

	defer func() {
		// Cancel the context in case it wasn't already canceled
		cancel()
		// Shutdown the Loopback HardwarePlugin server
		slog.Info("Shutting down Loopback HardwarePlugin server")
		if err := common.GracefulShutdown(srv); err != nil {
			slog.Error("error shutting down Loopback HardwarePlugin server", "error", err)
		}
	}()

	// Blocking select
	select {
	case err := <-serverErrors:
		return fmt.Errorf("error starting Loopback HardwarePlugin server: %w", err)
	case <-ctx.Done():
		slog.Info("Process shutting down Loopback HardwarePlugin server")
	}

	return nil
}
