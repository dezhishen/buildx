package bake

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/compose-spec/compose-go/v2/consts"
	"github.com/compose-spec/compose-go/v2/dotenv"
	"github.com/compose-spec/compose-go/v2/loader"
	composeschema "github.com/compose-spec/compose-go/v2/schema"
	composetypes "github.com/compose-spec/compose-go/v2/types"
	"github.com/docker/buildx/util/buildflags"
	dockeropts "github.com/docker/cli/opts"
	"github.com/docker/go-units"
	"github.com/pkg/errors"
	"go.yaml.in/yaml/v3"
)

func ParseComposeFiles(fs []File) (*Config, error) {
	envs, err := composeEnv()
	if err != nil {
		return nil, err
	}
	var cfgs []composetypes.ConfigFile
	for _, f := range fs {
		cfgs = append(cfgs, composetypes.ConfigFile{
			Filename: f.Name,
			Content:  f.Data,
		})
	}
	return ParseCompose(cfgs, envs)
}

func ParseCompose(cfgs []composetypes.ConfigFile, envs map[string]string) (*Config, error) {
	cfg, err := loadComposeFiles(cfgs, envs)
	if err != nil {
		return nil, err
	}

	var c Config
	if len(cfg.Services) > 0 {
		c.Groups = []*Group{}
		c.Targets = []*Target{}

		g := &Group{Name: "default"}

		for _, s := range cfg.Services {
			if s.Build == nil {
				continue
			}

			targetName := sanitizeTargetName(s.Name)
			if err = validateTargetName(targetName); err != nil {
				return nil, errors.Wrapf(err, "invalid service name %q", targetName)
			}

			var contextPathP *string
			if s.Build.Context != "" {
				contextPath := s.Build.Context
				contextPathP = &contextPath
			}
			var dockerfilePathP *string
			if s.Build.Dockerfile != "" {
				dockerfilePath := s.Build.Dockerfile
				dockerfilePathP = &dockerfilePath
			}
			var dockerfileInlineP *string
			if s.Build.DockerfileInline != "" {
				dockerfileInline := s.Build.DockerfileInline
				dockerfileInlineP = &dockerfileInline
			}

			var additionalContexts map[string]string
			if s.Build.AdditionalContexts != nil {
				additionalContexts = composeToBuildkitNamedContexts(s.Build.AdditionalContexts)
			}

			var shmSize *string
			if s.Build.ShmSize > 0 {
				shmSizeBytes := dockeropts.MemBytes(s.Build.ShmSize)
				shmSizeStr := shmSizeBytes.String()
				shmSize = &shmSizeStr
			}

			var networkModeP *string
			if s.Build.Network != "" {
				networkMode := s.Build.Network
				networkModeP = &networkMode
			}

			var ulimits []string
			if s.Build.Ulimits != nil {
				for n, u := range s.Build.Ulimits {
					ulimit, err := units.ParseUlimit(fmt.Sprintf("%s=%d:%d", n, u.Soft, u.Hard))
					if err != nil {
						return nil, err
					}
					ulimits = append(ulimits, ulimit.String())
				}
			}

			extraHosts := map[string]*string{}
			if s.Build.ExtraHosts != nil {
				for k, v := range s.Build.ExtraHosts {
					vv := strings.Join(v, ",")
					extraHosts[k] = &vv
				}
			}

			var ssh []*buildflags.SSH
			for _, bkey := range s.Build.SSH {
				sshkey := composeToBuildkitSSH(bkey)
				ssh = append(ssh, sshkey)
			}
			slices.SortFunc(ssh, func(a, b *buildflags.SSH) int {
				return a.Less(b)
			})

			var secrets []*buildflags.Secret
			for _, bs := range s.Build.Secrets {
				secret, err := composeToBuildkitSecret(bs, cfg.Secrets[bs.Source])
				if err != nil {
					return nil, err
				}
				secrets = append(secrets, secret)
			}

			// compose does not support nil values for labels
			labels := map[string]*string{}
			for k, v := range s.Build.Labels {
				labels[k] = &v
			}

			cacheFrom, err := buildflags.ParseCacheEntry(s.Build.CacheFrom)
			if err != nil {
				return nil, err
			}

			cacheTo, err := buildflags.ParseCacheEntry(s.Build.CacheTo)
			if err != nil {
				return nil, err
			}

			var inAttests []string
			if s.Build.SBOM != "" {
				inAttests = append(inAttests, buildflags.CanonicalizeAttest("sbom", s.Build.SBOM))
			}
			if s.Build.Provenance != "" {
				inAttests = append(inAttests, buildflags.CanonicalizeAttest("provenance", s.Build.Provenance))
			}
			attests, err := buildflags.ParseAttests(inAttests)
			if err != nil {
				return nil, err
			}

			g.Targets = append(g.Targets, targetName)
			t := &Target{
				Name:             targetName,
				Context:          contextPathP,
				Contexts:         additionalContexts,
				Dockerfile:       dockerfilePathP,
				DockerfileInline: dockerfileInlineP,
				Tags:             s.Build.Tags,
				Labels:           labels,
				Args: flatten(s.Build.Args.Resolve(func(val string) (string, bool) {
					if val, ok := s.Environment[val]; ok && val != nil {
						return *val, true
					}
					val, ok := cfg.Environment[val]
					return val, ok
				})),
				CacheFrom:   cacheFrom,
				CacheTo:     cacheTo,
				NetworkMode: networkModeP,
				Platforms:   s.Build.Platforms,
				SSH:         ssh,
				Secrets:     secrets,
				ShmSize:     shmSize,
				Ulimits:     ulimits,
				ExtraHosts:  extraHosts,
				Attest:      attests,
			}
			if err = t.composeExtTarget(s.Build.Extensions); err != nil {
				return nil, err
			}
			if s.Build.Target != "" {
				target := s.Build.Target
				t.Target = &target
			}
			if len(t.Tags) == 0 && s.Image != "" {
				t.Tags = []string{s.Image}
			}
			c.Targets = append(c.Targets, t)
		}
		c.Groups = append(c.Groups, g)
	}

	return &c, nil
}

func loadComposeFiles(cfgs []composetypes.ConfigFile, envs map[string]string, options ...func(*loader.Options)) (*composetypes.Project, error) {
	if envs == nil {
		envs = make(map[string]string)
	}

	cfgDetails := composetypes.ConfigDetails{
		ConfigFiles: cfgs,
		Environment: envs,
	}

	raw, err := loader.LoadModelWithContext(context.Background(), cfgDetails, append([]func(*loader.Options){func(opts *loader.Options) {
		projectName := "bake"
		if v, ok := envs[consts.ComposeProjectName]; ok && v != "" {
			projectName = v
		}
		opts.SetProjectName(projectName, false)
		opts.SkipNormalization = true
		opts.SkipValidation = true
	}}, options...)...)
	if err != nil {
		return nil, err
	}

	filtered := make(map[string]any)
	for _, key := range []string{"services", "secrets"} {
		if key == "services" {
			if services, ok := raw["services"].(map[string]any); ok {
				filteredServices := make(map[string]any)
				for svcName, svc := range services {
					if svc == nil {
						filteredServices[svcName] = map[string]any{}
					} else if svcMap, ok := svc.(map[string]any); ok {
						filteredService := make(map[string]any)
						for _, svcField := range []string{"image", "build", "environment", "env_file"} {
							if val, ok := svcMap[svcField]; ok {
								filteredService[svcField] = val
							}
						}
						filteredServices[svcName] = filteredService
					}
				}
				filtered["services"] = filteredServices
			}
		} else if v, ok := raw[key]; ok {
			filtered[key] = v
		}
	}
	if len(filtered) == 0 {
		return nil, errors.New("empty compose file")
	}

	if err := composeschema.Validate(filtered); err != nil {
		return nil, err
	}

	return loader.ModelToProject(filtered, loader.ToOptions(&cfgDetails, append([]func(*loader.Options){func(options *loader.Options) {
		options.SkipNormalization = true
		options.Profiles = []string{"*"}
	}}, options...)), composetypes.ConfigDetails{
		ConfigFiles: cfgs,
		Environment: envs,
	})
}

func validateComposeFile(dt []byte, fn string) (bool, error) {
	envs, err := composeEnv()
	if err != nil {
		return false, err
	}
	fnl := strings.ToLower(fn)
	if strings.HasSuffix(fnl, ".yml") || strings.HasSuffix(fnl, ".yaml") {
		return true, validateCompose(dt, envs)
	}
	if strings.HasSuffix(fnl, ".json") || strings.HasSuffix(fnl, ".hcl") {
		return false, nil
	}
	err = validateCompose(dt, envs)
	return err == nil, err
}

func validateCompose(dt []byte, envs map[string]string) error {
	_, err := loadComposeFiles([]composetypes.ConfigFile{{Content: dt}}, envs, func(options *loader.Options) {
		// consistency is checked later in ParseCompose to ensure multiple
		// compose files can be merged together
		options.SkipConsistencyCheck = true
	})
	return err
}

func composeEnv() (map[string]string, error) {
	envs := sliceToMap(os.Environ())
	if wd, err := os.Getwd(); err == nil {
		envs, err = loadDotEnv(envs, wd)
		if err != nil {
			return nil, err
		}
	}
	return envs, nil
}

func loadDotEnv(curenv map[string]string, workingDir string) (map[string]string, error) {
	if curenv == nil {
		curenv = make(map[string]string)
	}

	ef, err := filepath.Abs(filepath.Join(workingDir, ".env"))
	if err != nil {
		return nil, err
	}

	if st, err := os.Stat(ef); err != nil {
		if os.IsNotExist(err) {
			return curenv, nil
		}
		return nil, err
	} else if st.IsDir() {
		return curenv, nil
	}

	dt, err := os.ReadFile(ef)
	if err != nil {
		return nil, err
	}

	envs, err := dotenv.UnmarshalBytesWithLookup(dt, func(k string) (string, bool) {
		v, ok := curenv[k]
		return v, ok
	})
	if err != nil {
		return nil, err
	}

	for k, v := range envs {
		if _, set := curenv[k]; set {
			continue
		}
		curenv[k] = v
	}

	return curenv, nil
}

func flatten(in composetypes.MappingWithEquals) map[string]*string {
	if len(in) == 0 {
		return nil
	}
	out := map[string]*string{}
	for k, v := range in {
		if v == nil {
			continue
		}
		out[k] = v
	}
	return out
}

// xbake Compose build extension provides fields not (yet) available in
// Compose build specification: https://github.com/compose-spec/compose-spec/blob/master/build.md
type xbake struct {
	Tags          stringArray `yaml:"tags,omitempty"`
	CacheFrom     stringArray `yaml:"cache-from,omitempty"`
	CacheTo       stringArray `yaml:"cache-to,omitempty"`
	Secrets       stringArray `yaml:"secret,omitempty"`
	SSH           stringArray `yaml:"ssh,omitempty"`
	Platforms     stringArray `yaml:"platforms,omitempty"`
	Outputs       stringArray `yaml:"output,omitempty"`
	Pull          *bool       `yaml:"pull,omitempty"`
	NoCache       *bool       `yaml:"no-cache,omitempty"`
	NoCacheFilter stringArray `yaml:"no-cache-filter,omitempty"`
	Contexts      stringMap   `yaml:"contexts,omitempty"`
	// don't forget to update documentation if you add a new field:
	// https://github.com/docker/docs/blob/main/content/build/bake/compose-file.md#extension-field-with-x-bake
}

type (
	stringMap   map[string]string
	stringArray []string
)

func (sa *stringArray) UnmarshalYAML(unmarshal func(any) error) error {
	var multi []string
	err := unmarshal(&multi)
	if err != nil {
		var single string
		if err := unmarshal(&single); err != nil {
			return err
		}
		*sa = strings.Fields(single)
	} else {
		*sa = multi
	}
	return nil
}

// composeExtTarget converts Compose build extension x-bake to bake Target
// https://github.com/compose-spec/compose-spec/blob/master/spec.md#extension
func (t *Target) composeExtTarget(exts map[string]any) error {
	var xb xbake

	ext, ok := exts["x-bake"]
	if !ok || ext == nil {
		return nil
	}

	yb, _ := yaml.Marshal(ext)
	if err := yaml.Unmarshal(yb, &xb); err != nil {
		return err
	}

	if len(xb.Tags) > 0 {
		t.Tags = dedupSlice(append(t.Tags, xb.Tags...))
	}
	if len(xb.CacheFrom) > 0 {
		cacheFrom, err := buildflags.ParseCacheEntry(xb.CacheFrom)
		if err != nil {
			return err
		}
		t.CacheFrom = t.CacheFrom.Merge(cacheFrom)
	}
	if len(xb.CacheTo) > 0 {
		cacheTo, err := buildflags.ParseCacheEntry(xb.CacheTo)
		if err != nil {
			return err
		}
		t.CacheTo = t.CacheTo.Merge(cacheTo)
	}
	if len(xb.Secrets) > 0 {
		secrets, err := parseArrValue[buildflags.Secret](xb.Secrets)
		if err != nil {
			return err
		}
		t.Secrets = t.Secrets.Merge(secrets)
	}
	if len(xb.SSH) > 0 {
		ssh, err := parseArrValue[buildflags.SSH](xb.SSH)
		if err != nil {
			return err
		}
		t.SSH = t.SSH.Merge(ssh)
		slices.SortFunc(t.SSH, func(a, b *buildflags.SSH) int {
			return a.Less(b)
		})
	}
	if len(xb.Platforms) > 0 {
		t.Platforms = dedupSlice(append(t.Platforms, xb.Platforms...))
	}
	if len(xb.Outputs) > 0 {
		outputs, err := parseArrValue[buildflags.ExportEntry](xb.Outputs)
		if err != nil {
			return err
		}
		t.Outputs = t.Outputs.Merge(outputs)
	}
	if xb.Pull != nil {
		t.Pull = xb.Pull
	}
	if xb.NoCache != nil {
		t.NoCache = xb.NoCache
	}
	if len(xb.NoCacheFilter) > 0 {
		t.NoCacheFilter = dedupSlice(append(t.NoCacheFilter, xb.NoCacheFilter...))
	}
	if len(xb.Contexts) > 0 {
		t.Contexts = dedupMap(t.Contexts, composeToBuildkitNamedContexts(xb.Contexts))
	}

	return nil
}

// composeToBuildkitSecret converts secret from compose format to buildkit's
// csv format.
func composeToBuildkitSecret(inp composetypes.ServiceSecretConfig, psecret composetypes.SecretConfig) (*buildflags.Secret, error) {
	if psecret.External {
		return nil, errors.Errorf("unsupported external secret %s", psecret.Name)
	}

	secret := &buildflags.Secret{}
	if inp.Source != "" {
		secret.ID = inp.Source
	}
	if psecret.File != "" {
		secret.FilePath = psecret.File
	}
	if psecret.Environment != "" {
		secret.Env = psecret.Environment
	}
	return secret, nil
}

// composeToBuildkitSSH converts secret from compose format to buildkit's
// csv format.
func composeToBuildkitSSH(sshKey composetypes.SSHKey) *buildflags.SSH {
	bkssh := &buildflags.SSH{ID: sshKey.ID}
	if sshKey.Path != "" {
		bkssh.Paths = []string{sshKey.Path}
	}
	return bkssh
}

func composeToBuildkitNamedContexts(m map[string]string) map[string]string {
	out := make(map[string]string, len(m))
	for k, v := range m {
		if strings.HasPrefix(v, "service:") || strings.HasPrefix(v, "target:") {
			if parts := strings.SplitN(v, ":", 2); len(parts) == 2 {
				v = "target:" + sanitizeTargetName(parts[1])
			}
		}
		out[k] = v
	}
	return out
}
