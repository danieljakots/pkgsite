// Copyright 2021 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:generate go run gen_query.go

// Package symbolsearch provides helper functions for constructing queries for
// symbol search, which are using in internal/postgres.
//
// The exported queries are generated using gen_query.go. query.gen.go should
// never be edited directly. It should always be recreated by running
// `go generate -run gen_query.go`.
package symbolsearch

import (
	"fmt"
	"strings"
)

const SymbolTextSearchConfiguration = "symbols"

var (
	rawLegacyQuerySymbol           = constructQuery(filterSymbol)
	rawLegacyQueryPackageDotSymbol = constructQuery(filterPackageDotSymbol)
	rawLegacyQueryMultiWord        = constructQuery(filterMultiWord)
)

// constructQuery is used to construct a symbol search query.
func constructQuery(where string) string {
	// When there is only one word in the query, popularity is the only score
	// that matters.
	score := popularityMultiplier
	if where == filterMultiWord {
		score = formatScore(scoreMultiWord)
	}
	return fmt.Sprintf(symbolSearchBaseQuery, score, where)
}

var (
	// filterSymbol is used when $1 is the full symbol name, either
	// <symbol> or <type>.<methodOrField>.
	filterSymbol = fmt.Sprintf(`s.tsv_name_tokens @@ %s`, toTSQuery("$1"))

	// filterSymbol is used when $1 contains the full symbol name, either
	// <symbol> or <type>.<methodOrField>, and has multiple words.
	filterSymbolOR = fmt.Sprintf(`s.tsv_name_tokens @@ %s`, toTSQuery(splitOR))

	// filterPackageDotSymbol is used when $1 is either <package>.<symbol> OR
	// <package>.<type>.<methodOrField>.
	filterPackageDotSymbol = fmt.Sprintf("%s AND %s",
		filterPackageNameOrPath,
		fmt.Sprintf(formatFilter("s.tsv_name_tokens @@ %s"),
			toTSQuery("substring($1 from E'[^.]*\\.(.+)$')")))

	filterPackageNameOrPath = fmt.Sprintf(
		"(sd.name=%s OR sd.package_path=%[1]s)", splitFirstDot)

	// filterPackage is used to filter matching elements from
	// sd.tsv_path_tokens.
	filterPackage = fmt.Sprintf(`sd.tsv_path_tokens @@ %s`, toTSQuery(splitOR))

	// filterMultiWord when $1 contains multiple words, separated by spaces.
	// One element for the query must match a symbol name, and one (could be
	// the same element) must match the package name.
	filterMultiWord = fmt.Sprintf("%s AND %s", formatFilter(filterSymbolOR),
		formatFilter(filterPackage))
)

var (
	// scoreMultiWord is the score when $1 contains multiple words.
	scoreMultiWord = fmt.Sprintf("%s%s", rankPathTokens, formatMultiplier(popularityMultiplier))

	rankPathTokens = fmt.Sprintf(
		"ts_rank(%s,%s,%s"+indent(")", 3),
		indent("'{0.1, 0.2, 1.0, 1.0}'", 4),
		indent("sd.tsv_path_tokens", 4),
		indent(toTSQuery(splitOR), 4))

	// Popularity multipler to increase ranking of popular packages.
	popularityMultiplier = `ln(exp(1)+sd.imported_by_count)`
)

func formatScore(s string) string {
	return fmt.Sprintf("(\n\t\t\t\t%s\n\t\t\t)", s)
}

func formatFilter(s string) string {
	return fmt.Sprintf("(\n\t\t\t%s\n\t\t)", s)
}

func formatMultiplier(s string) string {
	return indent(fmt.Sprintf("* %s", s), 3)
}

func indent(s string, n int) string {
	for i := 0; i <= n; i++ {
		s = "\t" + s
	}
	return "\n" + s
}

const (
	splitOR = "replace($1, ' ', ' | ')"

	// splitFirstDot splits everything preceding the first dot in $1.
	// This is used to parse th package name or path.
	splitFirstDot = "split_part($1, '.', 1)"
)

func toTSQuery(arg string) string {
	return fmt.Sprintf("to_tsquery('%s', %s)", SymbolTextSearchConfiguration, processArg(arg))
}

// processArg converts a symbol with underscores to slashes (for example,
// "A_B" -> "A-B"). This is because the postgres parser treats underscores as
// slashes, but we want a search for "A" to rank "A_B" lower than just "A". We
// also want to be able to search specificially for "A_B".
func processArg(arg string) string {
	s := "$1"
	if len(arg) == 2 && strings.HasPrefix(arg, "$") {
		// If the arg is a different $N, substitute that instead.
		s = arg
	}
	return strings.ReplaceAll(arg, s, fmt.Sprintf("replace(%s, '_', '-')", s))
}

const symbolSearchBaseQuery = `
WITH results AS (
	SELECT
			s.name AS symbol_name,
			sd.package_path,
			sd.module_path,
			sd.version,
			sd.name AS package_name,
			sd.synopsis,
			sd.license_types,
			sd.commit_time,
			sd.imported_by_count,
			ssd.package_symbol_id,
			ssd.goos,
			ssd.goarch,
			%s AS score
	FROM symbol_search_documents ssd
	INNER JOIN search_documents sd ON sd.unit_id = ssd.unit_id
	INNER JOIN symbol_names s ON s.id = ssd.symbol_name_id
	WHERE %s
)
SELECT
	r.symbol_name,
	r.package_path,
	r.module_path,
	r.version,
	r.package_name,
	r.synopsis,
	r.license_types,
	r.commit_time,
	r.imported_by_count,
	r.goos,
	r.goarch,
	ps.type AS symbol_type,
	ps.synopsis AS symbol_synopsis
FROM results r
INNER JOIN package_symbols ps ON r.package_symbol_id = ps.id
WHERE r.score > 0.1
ORDER BY
	score DESC,
	commit_time DESC,
	symbol_name,
	package_path
LIMIT $2;`
