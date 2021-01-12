//
// Copyright (C) 2019-2021 vdaas.org vald team <vald@vdaas.org>
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//    https://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.
//

package usecase

import (
	"context"

	"github.com/vdaas/vald/apis/grpc/v1/meta"
	iconf "github.com/vdaas/vald/internal/config"
	"github.com/vdaas/vald/internal/db/nosql/cassandra"
	"github.com/vdaas/vald/internal/errgroup"
	"github.com/vdaas/vald/internal/net/grpc"
	"github.com/vdaas/vald/internal/net/grpc/metric"
	"github.com/vdaas/vald/internal/observability"
	dbmetrics "github.com/vdaas/vald/internal/observability/metrics/db/nosql/cassandra"
	"github.com/vdaas/vald/internal/runner"
	"github.com/vdaas/vald/internal/safety"
	"github.com/vdaas/vald/internal/servers/server"
	"github.com/vdaas/vald/internal/servers/starter"
	"github.com/vdaas/vald/pkg/meta/cassandra/config"
	handler "github.com/vdaas/vald/pkg/meta/cassandra/handler/grpc"
	"github.com/vdaas/vald/pkg/meta/cassandra/handler/rest"
	"github.com/vdaas/vald/pkg/meta/cassandra/router"
	"github.com/vdaas/vald/pkg/meta/cassandra/service"
)

type run struct {
	eg            errgroup.Group
	cfg           *config.Data
	c             service.Cassandra
	server        starter.Server
	observability observability.Observability
}

func New(cfg *config.Data) (r runner.Runner, err error) {
	cassandraOpts, err := cfg.Cassandra.Opts()
	if err != nil {
		return nil, err
	}

	var queryObserver dbmetrics.Observer
	if cfg.Observability.Enabled {
		queryObserver, err = dbmetrics.New()
		if err != nil {
			return nil, err
		}

		cassandraOpts = append(
			cassandraOpts,
			cassandra.WithQueryObserver(queryObserver),
		)
	}

	db, err := cassandra.New(cassandraOpts...)
	if err != nil {
		return nil, err
	}

	c, err := service.New(
		service.WithCassandra(db),
		service.WithKVTable(cfg.Cassandra.KVTable),
		service.WithVKTable(cfg.Cassandra.VKTable),
	)
	if err != nil {
		return nil, err
	}
	g := handler.New(handler.WithCassandra(c))
	eg := errgroup.Get()

	grpcServerOptions := []server.Option{
		server.WithGRPCRegistFunc(func(srv *grpc.Server) {
			meta.RegisterMetaServer(srv, g)
		}),
		server.WithGRPCOption(
			grpc.ChainUnaryInterceptor(grpc.RecoverInterceptor()),
			grpc.ChainStreamInterceptor(grpc.RecoverStreamInterceptor()),
		),
		server.WithPreStartFunc(func() error {
			// TODO check unbackupped upstream
			return nil
		}),
		server.WithPreStopFunction(func() error {
			// TODO backup all index data here
			return nil
		}),
	}

	var obs observability.Observability
	if cfg.Observability.Enabled {
		obs, err = observability.NewWithConfig(
			cfg.Observability,
			queryObserver,
		)
		if err != nil {
			return nil, err
		}
		grpcServerOptions = append(
			grpcServerOptions,
			server.WithGRPCOption(
				grpc.StatsHandler(metric.NewServerHandler()),
			),
		)
	}

	srv, err := starter.New(
		starter.WithConfig(cfg.Server),
		starter.WithREST(func(sc *iconf.Server) []server.Option {
			return []server.Option{
				server.WithHTTPHandler(
					router.New(
						router.WithTimeout(sc.HTTP.HandlerTimeout),
						router.WithErrGroup(eg),
						router.WithHandler(
							rest.New(
								rest.WithMeta(g),
							),
						),
					)),
			}
		}),
		starter.WithGRPC(func(sc *iconf.Server) []server.Option {
			return grpcServerOptions
		}),
		// TODO add GraphQL handler
	)
	if err != nil {
		return nil, err
	}

	return &run{
		eg:            eg,
		cfg:           cfg,
		c:             c,
		server:        srv,
		observability: obs,
	}, nil
}

func (r *run) PreStart(ctx context.Context) error {
	err := r.c.Connect(ctx)
	if err != nil {
		return err
	}
	if r.observability != nil {
		return r.observability.PreStart(ctx)
	}
	return nil
}

func (r *run) Start(ctx context.Context) (<-chan error, error) {
	ech := make(chan error, 2)
	var oech, sech <-chan error
	r.eg.Go(safety.RecoverFunc(func() (err error) {
		defer close(ech)
		if r.observability != nil {
			oech = r.observability.Start(ctx)
		}
		sech = r.server.ListenAndServe(ctx)
		for {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case err = <-oech:
			case err = <-sech:
			}
			if err != nil {
				select {
				case <-ctx.Done():
					return ctx.Err()
				case ech <- err:
				}
			}
		}
	}))
	return ech, nil
}

func (r *run) PreStop(ctx context.Context) error {
	return nil
}

func (r *run) Stop(ctx context.Context) error {
	if r.observability != nil {
		r.observability.Stop(ctx)
	}
	return r.server.Shutdown(ctx)
}

func (r *run) PostStop(ctx context.Context) error {
	return r.c.Close(ctx)
}
