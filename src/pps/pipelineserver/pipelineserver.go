package pipelineserver

import (
	"github.com/pachyderm/pachyderm/src/pkg/shard"
	"github.com/pachyderm/pachyderm/src/pps"
	"github.com/pachyderm/pachyderm/src/pps/persist"
)

type APIServer interface {
	shard.Frontend
	shard.Server
	pps.PipelineAPIServer
	Start() error
}

func NewAPIServer(
	pfsAddress string,
	jobAPIClient pps.JobAPIClient,
	persistAPIServer persist.APIServer,
) APIServer {
	return newAPIServer(
		pfsAddress,
		jobAPIClient,
		persistAPIServer,
	)
}
