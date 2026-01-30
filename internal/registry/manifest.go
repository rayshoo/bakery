package registry

import (
	"context"
	"fmt"

	"github.com/rayshoo/bakery/internal/state"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/empty"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/google/go-containerregistry/pkg/v1/types"
)

// PlatformImage 는 특정 아키텍처의 이미지 정보를 담는 구조체입니다.
type PlatformImage struct {
	Arch   string
	Image  string
	Digest string
}

// CreateManifestList 는 여러 아키텍처 이미지를 묶어 multi-arch manifest list 를 생성하고 registry 에 push 합니다.
func CreateManifestList(
	ctx context.Context,
	st *state.BuildState,
	images []PlatformImage,
	targetTag string,
) error {

	st.AppendLog("info", fmt.Sprintf("creating manifest list for %s", targetTag))

	adds := make([]mutate.IndexAddendum, 0, len(images))

	for _, img := range images {
		ref, err := name.ParseReference(img.Image, name.WeakValidation)
		if err != nil {
			return fmt.Errorf("parse image %s: %w", img.Image, err)
		}

		st.AppendLog("debug", fmt.Sprintf("  fetching %s", ref.String()))

		remoteImg, err := remote.Image(ref, remote.WithAuthFromKeychain(authn.DefaultKeychain))
		if err != nil {
			return fmt.Errorf("fetch image %s: %w", ref.String(), err)
		}

		platform, err := getPlatformForArch(img.Arch)
		if err != nil {
			return err
		}

		adds = append(adds, mutate.IndexAddendum{
			Add: remoteImg,
			Descriptor: v1.Descriptor{
				Platform: platform,
			},
		})

		st.AppendLog("debug", fmt.Sprintf("  added %s/%s", platform.OS, platform.Architecture))
	}

	idx := mutate.AppendManifests(
		mutate.IndexMediaType(empty.Index, types.DockerManifestList),
		adds...,
	)

	targetRef, err := name.ParseReference(targetTag, name.WeakValidation)
	if err != nil {
		return fmt.Errorf("parse target tag %s: %w", targetTag, err)
	}

	st.AppendLog("info", fmt.Sprintf("pushing manifest list to %s", targetRef.String()))

	if err := remote.WriteIndex(targetRef, idx, remote.WithAuthFromKeychain(authn.DefaultKeychain)); err != nil {
		return fmt.Errorf("push manifest list: %w", err)
	}

	digest, err := idx.Digest()
	if err != nil {
		return fmt.Errorf("get digest: %w", err)
	}

	st.AppendLog("info", fmt.Sprintf("manifest list pushed: %s", digest.String()))

	return nil
}

// getPlatformForArch 는 아키텍처 문자열을 v1.Platform 구조체로 변환합니다.
func getPlatformForArch(arch string) (*v1.Platform, error) {
	switch arch {
	case "amd64":
		return &v1.Platform{
			OS:           "linux",
			Architecture: "amd64",
		}, nil
	case "arm64":
		return &v1.Platform{
			OS:           "linux",
			Architecture: "arm64",
			Variant:      "v8",
		}, nil
	case "arm":
		return &v1.Platform{
			OS:           "linux",
			Architecture: "arm",
			Variant:      "v7",
		}, nil
	case "386":
		return &v1.Platform{
			OS:           "linux",
			Architecture: "386",
		}, nil
	case "ppc64le":
		return &v1.Platform{
			OS:           "linux",
			Architecture: "ppc64le",
		}, nil
	case "s390x":
		return &v1.Platform{
			OS:           "linux",
			Architecture: "s390x",
		}, nil
	default:
		return nil, fmt.Errorf("unsupported arch: %s", arch)
	}
}
