/*
Copyright 2026 Enrico Bianchi.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

// Package silo starts Silo (https://silo.pgsty.com), an S3-compatible
// object store forked from MinIO, in a container through testcontainers,
// for the tests of Kubebird's s3 backup destination.
package silo

import (
	"context"
	"fmt"

	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"
)

const (
	// Image is the Silo image the tests run.
	Image = "pgsty/silo:RELEASE.2026-11-12T00-00-00Z"

	// AccessKey and SecretKey are the root credentials Silo is started
	// with, which the tests use as S3 credentials.
	AccessKey = "kubebird"
	SecretKey = "kubebird-secret"

	// Port is Silo's S3 API port.
	Port = 9000
)

// Run starts a Silo container, waiting for its S3 API to be healthy.
// Extra options (e.g. the network to join) are applied after the defaults.
func Run(ctx context.Context, opts ...testcontainers.ContainerCustomizer) (*testcontainers.DockerContainer, error) {
	port := fmt.Sprintf("%d/tcp", Port)
	defaults := []testcontainers.ContainerCustomizer{
		testcontainers.WithEnv(map[string]string{
			"MINIO_ROOT_USER":     AccessKey,
			"MINIO_ROOT_PASSWORD": SecretKey,
		}),
		testcontainers.WithCmd("server", "/data"),
		testcontainers.WithExposedPorts(port),
		testcontainers.WithWaitStrategy(wait.ForHTTP("/minio/health/live").WithPort(port)),
	}
	c, err := testcontainers.Run(ctx, Image, append(defaults, opts...)...)
	if err != nil {
		return c, fmt.Errorf("failed to start Silo: %w", err)
	}
	return c, nil
}
