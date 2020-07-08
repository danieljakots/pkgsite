// Copyright 2019 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package frontend

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"golang.org/x/pkgsite/internal"
	"golang.org/x/pkgsite/internal/derrors"
	"golang.org/x/pkgsite/internal/log"
	"golang.org/x/pkgsite/internal/postgres"
	"golang.org/x/pkgsite/internal/stdlib"
)

// handlePackageDetailsRedirect redirects all redirects to "/pkg" to "/".
func (s *Server) handlePackageDetailsRedirect(w http.ResponseWriter, r *http.Request) {
	urlPath := strings.TrimPrefix(r.URL.Path, "/pkg")
	http.Redirect(w, r, urlPath, http.StatusMovedPermanently)
}

// legacyServePackagePage serves details pages for the package with import path
// pkgPath, in the module specified by modulePath and version.
func (s *Server) legacyServePackagePage(w http.ResponseWriter, r *http.Request, pkgPath, modulePath, requestedVersion, resolvedVersion string) (err error) {
	ctx := r.Context()

	// This function handles top level behavior related to the existence of the
	// requested pkgPath@version.
	//   1. If a package exists at this version, serve it.
	//   2. If there is a directory at this version, serve it.
	//   3. If there is another version that contains this package path: serve a
	//      404 and suggest these versions.
	//   4. Just serve a 404
	pkg, err := s.ds.LegacyGetPackage(ctx, pkgPath, modulePath, resolvedVersion)
	if err == nil {
		return s.legacyServePackagePageWithPackage(ctx, w, r, pkg, requestedVersion)
	}
	if !errors.Is(err, derrors.NotFound) {
		return err
	}
	if requestedVersion == internal.LatestVersion {
		// If we've already checked the latest version, then we know that this path
		// is not a package at any version, so just skip ahead and serve the
		// directory page.
		dbDir, err := s.ds.LegacyGetDirectory(ctx, pkgPath, modulePath, resolvedVersion, internal.AllFields)
		if err != nil {
			if errors.Is(err, derrors.NotFound) {
				return pathNotFoundError(ctx, "package", pkgPath, requestedVersion)
			}
			return err
		}
		return s.legacyServeDirectoryPage(ctx, w, r, dbDir, requestedVersion)
	}
	dir, err := s.ds.LegacyGetDirectory(ctx, pkgPath, modulePath, resolvedVersion, internal.AllFields)
	if err == nil {
		return s.legacyServeDirectoryPage(ctx, w, r, dir, requestedVersion)
	}
	if !errors.Is(err, derrors.NotFound) {
		// The only error we expect is NotFound, so serve an 500 here, otherwise
		// whatever response we resolve below might be inconsistent or misleading.
		return fmt.Errorf("checking for directory: %v", err)
	}
	_, err = s.ds.LegacyGetPackage(ctx, pkgPath, modulePath, internal.LatestVersion)
	if err == nil {
		return pathFoundAtLatestError(ctx, "package", pkgPath, requestedVersion)
	}
	if !errors.Is(err, derrors.NotFound) {
		// Unlike the error handling for LegacyGetDirectory above, we don't serve an
		// InternalServerError here. The reasoning for this is that regardless of
		// the result of LegacyGetPackage(..., "latest"), we're going to serve a NotFound
		// response code. So the semantics of the endpoint are the same whether or
		// not we get an unexpected error from GetPackage -- we just don't serve a
		// more informative error response.
		log.Errorf(ctx, "error checking for latest package: %v", err)
		return nil
	}
	return pathNotFoundError(ctx, "package", pkgPath, requestedVersion)
}

func (s *Server) legacyServePackagePageWithPackage(ctx context.Context, w http.ResponseWriter, r *http.Request, pkg *internal.LegacyVersionedPackage, requestedVersion string) (err error) {
	defer func() {
		if _, ok := err.(*serverError); !ok {
			derrors.Wrap(&err, "legacyServePackagePageWithPackage(w, r, %q, %q, %q)", pkg.Path, pkg.ModulePath, requestedVersion)
		}
	}()
	pkgHeader, err := legacyCreatePackage(&pkg.LegacyPackage, &pkg.ModuleInfo, requestedVersion == internal.LatestVersion)
	if err != nil {
		return fmt.Errorf("creating package header for %s@%s: %v", pkg.Path, pkg.Version, err)
	}

	tab := r.FormValue("tab")
	settings, ok := packageTabLookup[tab]
	if !ok {
		var tab string
		if pkg.LegacyPackage.IsRedistributable {
			tab = "doc"
		} else {
			tab = "overview"
		}
		http.Redirect(w, r, fmt.Sprintf(r.URL.Path+"?tab=%s", tab), http.StatusFound)
		return nil
	}
	canShowDetails := pkg.LegacyPackage.IsRedistributable || settings.AlwaysShowDetails

	var details interface{}
	if canShowDetails {
		var err error
		details, err = fetchDetailsForPackage(ctx, r, tab, s.ds, pkg)
		if err != nil {
			return fmt.Errorf("fetching page for %q: %v", tab, err)
		}
	}
	page := &DetailsPage{
		basePage: s.newBasePage(r, packageHTMLTitle(&pkg.LegacyPackage)),
		Title:    packageTitle(&pkg.LegacyPackage),
		Settings: settings,
		Header:   pkgHeader,
		Breadcrumb: breadcrumbPath(pkgHeader.Path, pkgHeader.Module.ModulePath,
			pkgHeader.Module.LinkVersion),
		Details:        details,
		CanShowDetails: canShowDetails,
		Tabs:           packageTabSettings,
		PageType:       "pkg",
	}
	s.servePage(ctx, w, settings.TemplateName, page)
	return nil
}

// stdlibPathForShortcut returns a path in the stdlib that shortcut should redirect to,
// or the empty string if there is no such path.
func (s *Server) stdlibPathForShortcut(ctx context.Context, shortcut string) (path string, err error) {
	defer derrors.Wrap(&err, "stdlibPathForShortcut(ctx, %q)", shortcut)
	if !stdlib.Contains(shortcut) {
		return "", nil
	}
	db, ok := s.ds.(*postgres.DB)
	if !ok {
		return "", proxydatasourceNotSupportedErr()
	}
	matches, err := db.GetStdlibPathsWithSuffix(ctx, shortcut)
	if err != nil {
		return "", err
	}
	if len(matches) == 1 {
		return matches[0], nil
	}
	// No matches, or ambiguous.
	return "", nil
}

func (s *Server) servePackagePageWithVersionedDirectory(ctx context.Context,
	w http.ResponseWriter, r *http.Request, vdir *internal.VersionedDirectory, requestedVersion string) error {
	pkgHeader, err := createPackageNew(vdir, requestedVersion == internal.LatestVersion)
	if err != nil {
		return fmt.Errorf("creating package header for %s@%s: %v", vdir.Path, vdir.Version, err)
	}

	tab := r.FormValue("tab")
	settings, ok := packageTabLookup[tab]
	if !ok {
		var tab string
		if vdir.DirectoryNew.IsRedistributable {
			tab = "doc"
		} else {
			tab = "overview"
		}
		http.Redirect(w, r, fmt.Sprintf(r.URL.Path+"?tab=%s", tab), http.StatusFound)
		return nil
	}
	canShowDetails := vdir.DirectoryNew.IsRedistributable || settings.AlwaysShowDetails

	var details interface{}
	if canShowDetails {
		var err error
		details, err = fetchDetailsForVersionedDirectory(ctx, r, tab, s.ds, vdir)
		if err != nil {
			return fmt.Errorf("fetching page for %q: %v", tab, err)
		}
	}
	page := &DetailsPage{
		basePage: s.newBasePage(r, packageHTMLTitleNew(vdir.Package)),
		Title:    packageTitleNew(vdir.Package),
		Settings: settings,
		Header:   pkgHeader,
		Breadcrumb: breadcrumbPath(pkgHeader.Path, pkgHeader.Module.ModulePath,
			pkgHeader.Module.LinkVersion),
		Details:        details,
		CanShowDetails: canShowDetails,
		Tabs:           packageTabSettings,
		PageType:       "pkg",
	}
	s.servePage(ctx, w, settings.TemplateName, page)
	return nil
}
