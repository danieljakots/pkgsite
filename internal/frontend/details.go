// Copyright 2019 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package frontend

import (
	"context"
	"fmt"
	"html"
	"html/template"
	"log"
	"net/http"
	"net/url"
	"path/filepath"
	"strings"
	"time"

	"github.com/microcosm-cc/bluemonday"
	"github.com/russross/blackfriday/v2"
	"golang.org/x/discovery/internal"
	"golang.org/x/discovery/internal/derrors"
	"golang.org/x/discovery/internal/license"
	"golang.org/x/discovery/internal/middleware"
	"golang.org/x/discovery/internal/postgres"
	"golang.org/x/discovery/internal/thirdparty/module"
	"golang.org/x/discovery/internal/thirdparty/semver"
)

// TabSettings defines tab-specific metadata.
type TabSettings struct {
	// Name is the tab name used in the URL.
	Name string

	// DisplayName is the formatted tab name.
	DisplayName string

	// AlwaysShowDetails defines whether the tab content can be shown even if the
	// package is not determined to be redistributable.
	AlwaysShowDetails bool
}

// DetailsPage contains data for the doc template.
type DetailsPage struct {
	basePageData
	CanShowDetails bool
	Settings       TabSettings
	Details        interface{}
	PackageHeader  *Package
	Tabs           []TabSettings
}

// OverviewDetails contains all of the data that the overview template
// needs to populate.
type OverviewDetails struct {
	ModulePath string
	ReadMe     template.HTML
}

// DocumentationDetails contains data for the doc template.
type DocumentationDetails struct {
	ModulePath    string
	Documentation template.HTML
}

// ModuleDetails contains all of the data that the module template
// needs to populate.
type ModuleDetails struct {
	ModulePath string
	Version    string
	ReadMe     template.HTML
	Packages   []*Package
}

// ImportsDetails contains information for a package's imports.
type ImportsDetails struct {
	ModulePath string

	// ExternalImports is the collection of package imports that are not in
	// the Go standard library and are not part of the same module
	ExternalImports []string

	// InternalImports is an array of packages representing the package's
	// imports that are part of the same module.
	InternalImports []string

	// StdLib is an array of packages representing the package's imports
	// that are in the Go standard library.
	StdLib []string
}

// ImportedByDetails contains information for the collection of packages that
// import a given package.
type ImportedByDetails struct {
	ModulePath string

	// ExternalImportedBy is the collection of packages that import the
	// given package and are not part of the same module.
	ExternalImportedBy []string

	// InternalImportedBy is the collection of packages that import the given
	// package and are inside the same module.
	InternalImportedBy []string
}

// License contains information used for a single license section.
type License struct {
	*license.License
	Anchor string
}

// LicensesDetails contains license information for a package.
type LicensesDetails struct {
	Licenses []License
}

// LicenseMetadata contains license metadata that is used in the package
// header.
type LicenseMetadata struct {
	*license.Metadata
	Anchor string
}

// VersionsDetails contains all the data that the versions tab
// template needs to populate.
type VersionsDetails struct {
	Versions []*SeriesVersionGroup
}

// SeriesVersionGroup represents the series level of the versions list
// hierarchy.
type SeriesVersionGroup struct {
	Series        string
	Latest        *Package
	MajorVersions []*MajorVersionGroup
}

// MajorVersionGroup represents the major level of the versions
// list hierarchy (i.e. "v1").
type MajorVersionGroup struct {
	Major         string
	Latest        *Package
	MinorVersions []*MinorVersionGroup
}

// MinorVersionGroup represents the major/minor level of the versions
// list hierarchy (i.e. "1.5").
type MinorVersionGroup struct {
	Minor         string
	Latest        *Package
	PatchVersions []*Package
}

// Package contains information for an individual package.
type Package struct {
	Name              string
	Version           string
	Path              string
	ModulePath        string
	Synopsis          string
	CommitTime        string
	Title             string
	Licenses          []LicenseMetadata
	IsCommand         bool
	IsRedistributable bool
}

// Suffix returns the package path less the ModulePath prefix.
func (p *Package) Suffix() string {
	suffix := strings.TrimPrefix(strings.TrimPrefix(p.Path, p.ModulePath), "/")
	if suffix == "" {
		return p.Name
	}
	return suffix
}

// transformLicenseMetadata transforms license.Metadata into a LicenseMetadata
// by adding an anchor field.
func transformLicenseMetadata(dbLicenses []*license.Metadata) []LicenseMetadata {
	var licenseInfos []LicenseMetadata
	for _, l := range dbLicenses {
		licenseInfos = append(licenseInfos, LicenseMetadata{
			Metadata: l,
			Anchor:   licenseAnchor(l.FilePath),
		})
	}
	return licenseInfos
}

// createPackageHeader returns a *Package based on the fields of the specified
// package. It assumes that pkg and pkg.Version are not nil.
func createPackageHeader(pkg *internal.VersionedPackage) (*Package, error) {
	if pkg == nil {
		return nil, fmt.Errorf("package cannot be nil")
	}

	var isCmd bool
	if pkg.Name == "main" {
		isCmd = true
	}
	name := packageName(pkg.Name, pkg.Path)
	return &Package{
		Name:              name,
		IsCommand:         isCmd,
		Title:             packageTitle(name, isCmd),
		Version:           pkg.VersionInfo.Version,
		Path:              pkg.Path,
		Synopsis:          pkg.Package.Synopsis,
		Licenses:          transformLicenseMetadata(pkg.Licenses),
		CommitTime:        elapsedTime(pkg.VersionInfo.CommitTime),
		IsRedistributable: pkg.IsRedistributable(),
	}, nil
}

// inStdLib reports whether the package is part of the Go standard library.
func inStdLib(path string) bool {
	if i := strings.IndexByte(path, '/'); i != -1 {
		return !strings.Contains(path[:i], ".")
	}
	return !strings.Contains(path, ".")
}

// packageName returns name if it is not "main". Otherwise, it returns the last
// element of pkg.Path that is not a version identifier (such as "v2"). For
// example, if name is "main" and path is foo/bar/v2, name will be "bar".
func packageName(name, path string) string {
	if name != "main" {
		return name
	}

	if path[len(path)-3:] == "/v1" {
		return filepath.Base(path[:len(path)-3])
	}

	prefix, _, _ := module.SplitPathVersion(path)
	return filepath.Base(prefix)
}

// packageTitle returns name prefixed by "Command" if isCommand is true and
// "Package" if false.
func packageTitle(name string, isCommand bool) string {
	if isCommand {
		return fmt.Sprintf("Command %s", name)
	}
	return fmt.Sprintf("Package %s", name)
}

// elapsedTime takes a date and returns returns human-readable,
// relative timestamps based on the following rules:
// (1) 'X hours ago' when X < 6
// (2) 'today' between 6 hours and 1 day ago
// (3) 'Y days ago' when Y < 6
// (4) A date formatted like "Jan 2, 2006" for anything further back
func elapsedTime(date time.Time) string {
	elapsedHours := int(time.Since(date).Hours())
	if elapsedHours == 1 {
		return "1 hour ago"
	} else if elapsedHours < 6 {
		return fmt.Sprintf("%d hours ago", elapsedHours)
	}

	elapsedDays := elapsedHours / 24
	if elapsedDays < 1 {
		return "today"
	} else if elapsedDays == 1 {
		return "1 day ago"
	} else if elapsedDays < 6 {
		return fmt.Sprintf("%d days ago", elapsedDays)
	}

	return date.Format("Jan _2, 2006")
}

// fetchOverviewDetails fetches data for the module version specified by path and version
// from the database and returns a OverviewDetails.
func fetchOverviewDetails(ctx context.Context, db *postgres.DB, pkg *internal.VersionedPackage) (*OverviewDetails, error) {
	return &OverviewDetails{
		ModulePath: pkg.VersionInfo.ModulePath,
		ReadMe:     readmeHTML(pkg.VersionInfo.ReadmeFilePath, pkg.VersionInfo.ReadmeContents),
	}, nil
}

// fetchDocumentationDetails fetches data for the package specified by path and version
// from the database and returns a DocumentationDetails.
func fetchDocumentationDetails(ctx context.Context, db *postgres.DB, pkg *internal.VersionedPackage) (*DocumentationDetails, error) {
	return &DocumentationDetails{
		ModulePath:    pkg.VersionInfo.ModulePath,
		Documentation: template.HTML(pkg.DocumentationHTML),
	}, nil
}

// fetchModuleDetails fetches data for the module version specified by pkgPath and pkgversion
// from the database and returns a ModuleDetails.
func fetchModuleDetails(ctx context.Context, db *postgres.DB, pkg *internal.VersionedPackage) (*ModuleDetails, error) {
	version, err := db.GetVersionForPackage(ctx, pkg.Path, pkg.VersionInfo.Version)
	if err != nil {
		return nil, fmt.Errorf("db.GetVersionForPackage(ctx, %q, %q): %v", pkg.Path, pkg.VersionInfo.Version, err)
	}

	var packages []*Package
	for _, p := range version.Packages {
		packages = append(packages, &Package{
			Name:       p.Name,
			Path:       p.Path,
			Synopsis:   p.Synopsis,
			Licenses:   transformLicenseMetadata(p.Licenses),
			Version:    version.Version,
			ModulePath: version.ModulePath,
		})
	}

	return &ModuleDetails{
		ModulePath: version.ModulePath,
		Version:    pkg.VersionInfo.Version,
		ReadMe:     readmeHTML(version.ReadmeFilePath, version.ReadmeContents),
		Packages:   packages,
	}, nil
}

// fetchVersionsDetails fetches data for the module version specified by path
// and version from the database and returns a VersionsDetails. The package
// path for each SeriesVersionGroup is calculated as the module path and
// package suffix. For the standard library packages, it is the package path.
func fetchVersionsDetails(ctx context.Context, db *postgres.DB, pkg *internal.VersionedPackage) (*VersionsDetails, error) {
	versions, err := db.GetTaggedVersionsForPackageSeries(ctx, pkg.Path)
	if err != nil {
		return nil, fmt.Errorf("db.GetTaggedVersions(%q): %v", pkg.Path, err)
	}
	// If no tagged versions for the package series are found,
	// fetch the pseudo-versions instead.
	if len(versions) == 0 {
		versions, err = db.GetPseudoVersionsForPackageSeries(ctx, pkg.Path)
		if err != nil {
			return nil, fmt.Errorf("db.GetPseudoVersions(%q): %v", pkg.Path, err)
		}
	}

	var (
		svg       = []*SeriesVersionGroup{}
		curSeries *SeriesVersionGroup
		curMajor  *MajorVersionGroup
		curMinor  *MinorVersionGroup
	)
	for _, v := range versions {
		if v.SeriesPath() != pkg.SeriesPath() {
			if !strings.HasPrefix(pkg.V1Path, v.SeriesPath()) {
				log.Printf("got version with mismatching series: %q", v.SeriesPath())
				continue
			}
		}

		var pkgPath string
		suffix := strings.TrimPrefix(strings.TrimPrefix(pkg.V1Path, v.SeriesPath()), "/")
		if inStdLib(pkg.Path) {
			pkgPath = pkg.Path
		} else if suffix == "" {
			pkgPath = v.ModulePath
		} else {
			pkgPath = v.ModulePath + "/" + suffix
		}
		latest := &Package{
			Version:    v.Version,
			Path:       pkgPath,
			CommitTime: elapsedTime(v.CommitTime),
		}
		if series := v.SeriesPath(); curSeries == nil || curSeries.Series != series {
			curSeries = &SeriesVersionGroup{
				Series: series,
				Latest: latest,
			}
			svg = append(svg, curSeries)
			// nil-out curMajor and curMinor to force new version groups.
			curMajor = nil
			curMinor = nil
		}

		major := semver.Major(v.Version)
		if curMajor == nil || curMajor.Major != major {
			curMajor = &MajorVersionGroup{
				Major:  major,
				Latest: latest,
			}
			curSeries.MajorVersions = append(curSeries.MajorVersions, curMajor)
		}

		majMin := semver.MajorMinor(v.Version)
		if curMinor == nil || curMinor.Minor != majMin {
			curMinor = &MinorVersionGroup{
				Minor:  majMin,
				Latest: latest,
			}
			curMajor.MinorVersions = append(curMajor.MinorVersions, curMinor)
		}
		curMinor.PatchVersions = append(curMinor.PatchVersions, latest)
	}

	return &VersionsDetails{
		Versions: svg,
	}, nil
}

// licenseAnchor returns the anchor that should be used to jump to the specific
// license on the licenses page.
func licenseAnchor(filePath string) string {
	return url.QueryEscape(filePath)
}

// fetchLicensesDetails fetches license data for the package version specified by
// path and version from the database and returns a LicensesDetails.
func fetchLicensesDetails(ctx context.Context, db *postgres.DB, pkg *internal.VersionedPackage) (*LicensesDetails, error) {
	dbLicenses, err := db.GetLicenses(ctx, pkg.Path, pkg.VersionInfo.Version)
	if err != nil {
		return nil, fmt.Errorf("db.GetLicenses(ctx, %q, %q): %v", pkg.Path, pkg.VersionInfo.Version, err)
	}

	licenses := make([]License, len(dbLicenses))
	for i, l := range dbLicenses {
		licenses[i] = License{
			Anchor:  licenseAnchor(l.FilePath),
			License: l,
		}
	}

	return &LicensesDetails{
		Licenses: licenses,
	}, nil
}

// fetchImportsDetails fetches imports for the package version specified by
// path and version from the database and returns a ImportsDetails.
func fetchImportsDetails(ctx context.Context, db *postgres.DB, pkg *internal.VersionedPackage) (*ImportsDetails, error) {
	dbImports, err := db.GetImports(ctx, pkg.Path, pkg.VersionInfo.Version)
	if err != nil {
		return nil, fmt.Errorf("db.GetImports(ctx, %q, %q): %v", pkg.Path, pkg.VersionInfo.Version, err)
	}

	var externalImports, moduleImports, std []string
	for _, p := range dbImports {
		if inStdLib(p) {
			std = append(std, p)
		} else if strings.HasPrefix(p+"/", pkg.VersionInfo.ModulePath+"/") {
			moduleImports = append(moduleImports, p)
		} else {
			externalImports = append(externalImports, p)
		}
	}

	return &ImportsDetails{
		ModulePath:      pkg.VersionInfo.ModulePath,
		ExternalImports: externalImports,
		InternalImports: moduleImports,
		StdLib:          std,
	}, nil
}

// fetchImportedByDetails fetches importers for the package version specified by
// path and version from the database and returns a ImportedByDetails.
func fetchImportedByDetails(ctx context.Context, db *postgres.DB, pkg *internal.VersionedPackage) (*ImportedByDetails, error) {
	importedByPaths, err := db.GetImportedBy(ctx, pkg.Path)
	if err != nil {
		return nil, fmt.Errorf("db.GetImportedBy(ctx, %q): %v", pkg.Path, err)
	}
	var externalImportedBy, moduleImportedBy []string
	for _, path := range importedByPaths {
		if strings.HasPrefix(path, pkg.VersionInfo.ModulePath) {
			moduleImportedBy = append(moduleImportedBy, path)
		} else {
			externalImportedBy = append(externalImportedBy, path)
		}
	}
	return &ImportedByDetails{
		ModulePath:         pkg.VersionInfo.ModulePath,
		ExternalImportedBy: externalImportedBy,
		InternalImportedBy: moduleImportedBy,
	}, nil
}

// readmeHTML sanitizes readmeContents based on bluemondy.UGCPolicy and returns
// a template.HTML. If readmeFilePath indicates that this is a markdown file,
// it will also render the markdown contents using blackfriday.
func readmeHTML(readmeFilePath string, readmeContents []byte) template.HTML {
	if filepath.Ext(readmeFilePath) != ".md" {
		return template.HTML(fmt.Sprintf(`<pre class="readme">%s</pre>`, html.EscapeString(string(readmeContents))))
	}

	// bluemonday.UGCPolicy allows a broad selection of HTML elements and
	// attributes that are safe for user generated content. This policy does
	// not whitelist iframes, object, embed, styles, script, etc.
	p := bluemonday.UGCPolicy()
	unsafe := blackfriday.Run(readmeContents)
	return template.HTML(p.SanitizeBytes(unsafe))
}

var (
	tabSettings = []TabSettings{
		{
			Name:        "doc",
			DisplayName: "Doc",
		},
		{
			Name:        "overview",
			DisplayName: "Overview",
		},
		{
			Name:        "module",
			DisplayName: "Module",
		},
		{
			Name:              "versions",
			AlwaysShowDetails: true,
			DisplayName:       "Versions",
		},
		{
			Name:              "imports",
			DisplayName:       "Imports",
			AlwaysShowDetails: true,
		},
		{
			Name:              "importedby",
			DisplayName:       "Imported By",
			AlwaysShowDetails: true,
		},
		{
			Name:        "licenses",
			DisplayName: "Licenses",
		},
	}
	tabLookup = make(map[string]TabSettings)
)

func init() {
	for _, d := range tabSettings {
		tabLookup[d.Name] = d
	}
}

// fetchDetails returns tab details by delegating to the correct detail
// handler.
func fetchDetails(ctx context.Context, tab string, db *postgres.DB, pkg *internal.VersionedPackage) (interface{}, error) {
	switch tab {
	case "doc":
		return fetchDocumentationDetails(ctx, db, pkg)
	case "versions":
		return fetchVersionsDetails(ctx, db, pkg)
	case "module":
		return fetchModuleDetails(ctx, db, pkg)
	case "imports":
		return fetchImportsDetails(ctx, db, pkg)
	case "importedby":
		return fetchImportedByDetails(ctx, db, pkg)
	case "licenses":
		return fetchLicensesDetails(ctx, db, pkg)
	case "overview":
		return fetchOverviewDetails(ctx, db, pkg)
	}
	return nil, fmt.Errorf("BUG: unable to fetch details: unknown tab %q", tab)
}

// parseModulePathAndVersion returns the module and version specified by
// urlPath. urlPath is assumed to be a valid path following the structure
// /<module>@<version>. Any leading or trailing slashes in the module path are
// trimmed.
func parseModulePathAndVersion(urlPath string) (importPath, version string, err error) {
	parts := strings.Split(urlPath, "@")
	if len(parts) != 1 && len(parts) != 2 {
		return "", "", fmt.Errorf("malformed URL path %q", urlPath)
	}

	importPath = strings.TrimPrefix(parts[0], "/")
	if len(parts) == 1 {
		importPath = strings.TrimSuffix(importPath, "/")
	}
	if err := module.CheckImportPath(importPath); err != nil {
		return "", "", fmt.Errorf("malformed import path %q: %v", importPath, err)
	}

	if len(parts) == 1 {
		return importPath, "", nil
	}
	return importPath, strings.TrimRight(parts[1], "/"), nil
}

// HandleDetails applies database data to the appropriate template. Handles all
// endpoints that match "/" or "/<import-path>[@<version>?tab=<tab>]"
func (s *Server) handleDetails(w http.ResponseWriter, r *http.Request) {
	path, version, err := parseModulePathAndVersion(r.URL.Path)
	if err != nil {
		log.Printf("parseModulePathAndVersion(%q): %v", r.URL.Path, err)
		s.serveErrorPage(w, r, http.StatusBadRequest, nil)
		return
	}
	if version != "" && !semver.IsValid(version) {
		s.serveErrorPage(w, r, http.StatusBadRequest, &errorPage{
			Message: fmt.Sprintf("%q is not a valid semantic version.", version),
			SecondaryMessage: template.HTML(
				fmt.Sprintf(`To search for packages like %q, <a href="/search?q=%s">click here</a>.</p>`, path, path)),
		})
		return
	}

	var (
		pkg *internal.VersionedPackage
		ctx = r.Context()
	)
	if version == "" {
		pkg, err = s.db.GetLatestPackage(ctx, path)
	} else {
		pkg, err = s.db.GetPackage(ctx, path, version)
	}
	if err != nil {
		if !derrors.IsNotFound(err) {
			log.Printf("error getting package for %s@%s: %v", path, version, err)
			s.serveErrorPage(w, r, http.StatusInternalServerError, nil)
			return
		}
		if version == "" {
			// If the version is empty, it means that we already tried fetching
			// the latest version of the package, and it does not exist.
			s.serveErrorPage(w, r, http.StatusBadRequest, nil)
			return
		}
		// Get the latest package to check if any versions of
		// the package exists.
		_, latestErr := s.db.GetLatestPackage(ctx, path)
		if latestErr == nil {
			s.serveErrorPage(w, r, http.StatusBadRequest, &errorPage{
				Message: fmt.Sprintf("Package %s@%s is not available.", path, version),
				SecondaryMessage: template.HTML(
					fmt.Sprintf(`There are other versions of this package that are! To view them, <a href="/%s?tab=versions">click here</a>.</p>`, path)),
			})
			return
		}
		if !derrors.IsNotFound(latestErr) {
			// We succeeded in fetching with GetPackage, but failed in fetching
			// with GetLatestPackage.
			log.Printf("error getting package for %s: %v", path, latestErr)
		}
		s.serveErrorPage(w, r, http.StatusBadRequest, nil)
		return
	}

	version = pkg.VersionInfo.Version
	pkgHeader, err := createPackageHeader(pkg)
	if err != nil {
		log.Printf("error creating package header for %s@%s: %v", path, version, err)
		s.serveErrorPage(w, r, http.StatusInternalServerError, nil)
		return
	}

	tab := r.FormValue("tab")
	settings, ok := tabLookup[tab]
	if !ok {
		tab = "doc"
		settings = tabLookup["doc"]
	}
	canShowDetails := pkg.IsRedistributable() || settings.AlwaysShowDetails

	var details interface{}
	if canShowDetails {
		var err error
		details, err = fetchDetails(ctx, tab, s.db, pkg)
		if err != nil {
			log.Printf("error fetching page for %q: %v", tab, err)
			s.serveErrorPage(w, r, http.StatusInternalServerError, nil)
			return
		}
	}

	nonce, ok := middleware.GetNonce(r.Context())
	if !ok {
		log.Printf("middleware.GetNonce(r.Context()): nonce was not set")
	}
	page := &DetailsPage{
		basePageData: basePageData{
			Title: fmt.Sprintf("%s - %s", pkgHeader.Title, pkgHeader.Version),
			Query: strings.TrimSpace(r.FormValue("q")),
			Nonce: nonce,
		},
		Settings:       settings,
		PackageHeader:  pkgHeader,
		Details:        details,
		CanShowDetails: canShowDetails,
		Tabs:           tabSettings,
	}
	s.servePage(w, tab+".tmpl", page)
}
