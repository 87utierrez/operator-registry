package composite

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"

	"github.com/operator-framework/operator-registry/pkg/image"
	"k8s.io/apimachinery/pkg/util/yaml"
)

type BuilderMap map[string]Builder

type CatalogBuilderMap map[string]BuilderMap

type builderFunc func(BuilderConfig) Builder

type Template struct {
	catalogFile        io.Reader
	contributionFile   io.Reader
	validate           bool
	outputType         string
	registry           image.Registry
	registeredBuilders map[string]builderFunc
}

type TemplateOption func(t *Template)

func WithCatalogFile(catalogFile io.Reader) TemplateOption {
	return func(t *Template) {
		t.catalogFile = catalogFile
	}
}

func WithContributionFile(contribFile io.Reader) TemplateOption {
	return func(t *Template) {
		t.contributionFile = contribFile
	}
}

func WithOutputType(outputType string) TemplateOption {
	return func(t *Template) {
		t.outputType = outputType
	}
}

func WithRegistry(reg image.Registry) TemplateOption {
	return func(t *Template) {
		t.registry = reg
	}
}

func WithValidate(validate bool) TemplateOption {
	return func(t *Template) {
		t.validate = validate
	}
}

func NewTemplate(opts ...TemplateOption) *Template {
	temp := &Template{
		// Default registered builders when creating a new Template
		registeredBuilders: map[string]builderFunc{
			BasicBuilderSchema:  func(bc BuilderConfig) Builder { return NewBasicBuilder(bc) },
			SemverBuilderSchema: func(bc BuilderConfig) Builder { return NewSemverBuilder(bc) },
			RawBuilderSchema:    func(bc BuilderConfig) Builder { return NewRawBuilder(bc) },
			CustomBuilderSchema: func(bc BuilderConfig) Builder { return NewCustomBuilder(bc) },
		},
	}

	for _, opt := range opts {
		opt(temp)
	}

	return temp
}

type HttpGetter interface {
	Get(url string) (*http.Response, error)
}

// FetchCatalogConfig will fetch the catalog configuration file from the given path.
// The path can be a local file path OR a URL that returns the raw contents of the catalog
// configuration file.
// The filepath can be structured relative or as an absolute path
func FetchCatalogConfig(path string, httpGetter HttpGetter) (io.ReadCloser, error) {
	var tempCatalog io.ReadCloser
	catalogURI, err := url.ParseRequestURI(path)
	// Evalute local catalog config
	// URI parse will fail on relative filepaths
	// Check if path is an absolute filepath
	if err != nil || filepath.IsAbs(path) {
		tempCatalog, err = os.Open(path)
		if err != nil {
			return nil, fmt.Errorf("opening catalog config file %q: %v", path, err)
		}
	} else {
		// Evalute remote catalog config
		// If URi is valid, execute fetch
		tempResp, err := httpGetter.Get(catalogURI.String())
		if err != nil {
			return nil, fmt.Errorf("fetching remote catalog config file %q: %v", path, err)
		}
		tempCatalog = tempResp.Body
	}

	return tempCatalog, nil
}

// TODO(everettraven): do we need the context here? If so, how should it be used?
func (t *Template) Render(ctx context.Context, validate bool) error {

	catalogFile, err := t.parseCatalogsSpec()
	if err != nil {
		return err
	}

	contributionFile, err := t.parseContributionSpec()
	if err != nil {
		return err
	}

	catalogBuilderMap, err := t.newCatalogBuilderMap(catalogFile.Catalogs, t.outputType)
	if err != nil {
		return err
	}

	// TODO(everettraven): should we return aggregated errors?
	for _, component := range contributionFile.Components {
		if builderMap, ok := (*catalogBuilderMap)[component.Name]; ok {
			if builder, ok := builderMap[component.Strategy.Template.Schema]; ok {
				// run the builder corresponding to the schema
				err := builder.Build(ctx, t.registry, component.Destination.Path, component.Strategy.Template)
				if err != nil {
					return fmt.Errorf("building component %q: %w", component.Name, err)
				}

				if validate {
					// run the validation for the builder
					err = builder.Validate(ctx, component.Destination.Path)
					if err != nil {
						return fmt.Errorf("validating component %q: %w", component.Name, err)
					}
				}
			} else {
				return fmt.Errorf("building component %q: no builder found for template schema %q", component.Name, component.Strategy.Template.Schema)
			}
		} else {
			allowedComponents := []string{}
			for k := range *catalogBuilderMap {
				allowedComponents = append(allowedComponents, k)
			}
			return fmt.Errorf("building component %q: component does not exist in the catalog configuration. Available components are: %s", component.Name, allowedComponents)
		}
	}
	return nil
}

func (t *Template) builderForSchema(schema string, builderCfg BuilderConfig) (Builder, error) {
	builderFunc, ok := t.registeredBuilders[schema]
	if !ok {
		return nil, fmt.Errorf("unknown schema %q", schema)
	}

	return builderFunc(builderCfg), nil
}

func (t *Template) parseCatalogsSpec() (*CatalogConfig, error) {

	// get catalog configurations
	catalogConfig := &CatalogConfig{}
	catalogDoc := json.RawMessage{}
	catalogDecoder := yaml.NewYAMLOrJSONDecoder(t.catalogFile, 4096)
	err := catalogDecoder.Decode(&catalogDoc)
	if err != nil {
		return nil, fmt.Errorf("decoding catalog config: %v", err)
	}
	err = json.Unmarshal(catalogDoc, catalogConfig)
	if err != nil {
		return nil, fmt.Errorf("unmarshalling catalog config: %v", err)
	}

	if catalogConfig.Schema != CatalogSchema {
		return nil, fmt.Errorf("catalog configuration file has unknown schema, should be %q", CatalogSchema)
	}

	return catalogConfig, nil
}

func (t *Template) parseContributionSpec() (*CompositeConfig, error) {

	// parse data to composite config
	compositeConfig := &CompositeConfig{}
	compositeDoc := json.RawMessage{}
	compositeDecoder := yaml.NewYAMLOrJSONDecoder(t.contributionFile, 4096)
	err := compositeDecoder.Decode(&compositeDoc)
	if err != nil {
		return nil, fmt.Errorf("decoding composite config: %v", err)
	}
	err = json.Unmarshal(compositeDoc, compositeConfig)
	if err != nil {
		return nil, fmt.Errorf("unmarshalling composite config: %v", err)
	}

	if compositeConfig.Schema != CompositeSchema {
		return nil, fmt.Errorf("composite configuration file has unknown schema, should be %q", CompositeSchema)
	}

	return compositeConfig, nil
}

func (t *Template) newCatalogBuilderMap(catalogs []Catalog, outputType string) (*CatalogBuilderMap, error) {

	catalogBuilderMap := make(CatalogBuilderMap)

	// setup the builders for each catalog
	setupFailed := false
	setupErrors := map[string][]string{}
	for _, catalog := range catalogs {
		errs := []string{}
		// if catalog.Destination.BaseImage == "" {
		// 	errs = append(errs, "destination.baseImage must not be an empty string")
		// }

		if catalog.Destination.WorkingDir == "" {
			errs = append(errs, "destination.workingDir must not be an empty string")
		}

		// check for validation errors and skip builder creation if there are any errors
		if len(errs) > 0 {
			setupFailed = true
			setupErrors[catalog.Name] = errs
			continue
		}

		if _, ok := catalogBuilderMap[catalog.Name]; !ok {
			builderMap := make(BuilderMap)
			for _, schema := range catalog.Builders {
				builder, err := t.builderForSchema(schema, BuilderConfig{
					OutputType: outputType,
				})
				if err != nil {
					return nil, fmt.Errorf("getting builder %q for catalog %q: %v", schema, catalog.Name, err)
				}
				builderMap[schema] = builder
			}
			catalogBuilderMap[catalog.Name] = builderMap
		}
	}

	// if there were errors validating the catalog configuration then exit
	if setupFailed {
		//build the error message
		var errMsg string
		for cat, errs := range setupErrors {
			errMsg += fmt.Sprintf("\nCatalog %v:\n", cat)
			for _, err := range errs {
				errMsg += fmt.Sprintf("  - %v\n", err)
			}
		}
		return nil, fmt.Errorf("catalog configuration file field validation failed: %s", errMsg)
	}

	return &catalogBuilderMap, nil
}
