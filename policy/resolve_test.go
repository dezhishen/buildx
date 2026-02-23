package policy

import (
	"context"
	"testing"

	slsa1 "github.com/in-toto/in-toto-golang/in_toto/slsa_provenance/v1"
	"github.com/moby/buildkit/client/llb/sourceresolver"
	"github.com/moby/buildkit/solver/pb"
	"github.com/opencontainers/go-digest"
	ocispecs "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/stretchr/testify/require"
)

type stubSourceResolver struct {
	resolveFn func(context.Context, *pb.SourceOp, sourceresolver.Opt) (*sourceresolver.MetaResponse, error)
}

func (s stubSourceResolver) ResolveSourceMetadata(ctx context.Context, op *pb.SourceOp, opt sourceresolver.Opt) (*sourceresolver.MetaResponse, error) {
	return s.resolveFn(ctx, op, opt)
}

func TestResolveInputUnknownsResolvesMaterialField(t *testing.T) {
	inp := Input{
		Image: &Image{
			Provenance: &ImageProvenance{
				Materials: []Input{
					{Image: &Image{Ref: "docker.io/library/alpine:3.20"}},
				},
				materialsRaw: []slsa1.ResourceDescriptor{
					{URI: "pkg:docker/library/alpine@3.20?platform=linux/amd64"},
				},
			},
		},
	}
	inp.Image.Provenance.Materials[0].setUnknowns([]string{"input.image.hasProvenance"})

	resolver := stubSourceResolver{
		resolveFn: func(_ context.Context, op *pb.SourceOp, _ sourceresolver.Opt) (*sourceresolver.MetaResponse, error) {
			require.Equal(t, "docker-image://docker.io/library/alpine:3.20", op.Identifier)
			return &sourceresolver.MetaResponse{
				Op: op,
				Image: &sourceresolver.ResolveImageResponse{
					Digest: digest.Digest("sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"),
					AttestationChain: &sourceresolver.AttestationChain{
						AttestationManifest: digest.Digest("sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"),
					},
				},
			}, nil
		},
	}

	retry, next, err := ResolveInputUnknowns(
		context.Background(),
		&inp,
		&pb.SourceOp{Identifier: "docker-image://docker.io/library/busybox:latest"},
		[]string{"image.provenance.materials[0].image.hasProvenance"},
		&pb.Platform{OS: "linux", Architecture: "amd64"},
		&ocispecs.Platform{OS: "linux", Architecture: "amd64"},
		resolver,
		nil,
		nil,
	)
	require.NoError(t, err)
	require.True(t, retry)
	require.Nil(t, next)
	require.True(t, inp.Image.Provenance.Materials[0].Image.HasProvenance)
}
