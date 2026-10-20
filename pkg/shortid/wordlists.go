// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package shortid

// The #1756 final format ruling: session short IDs are 3-word
// grammatical phrases (adjective-noun-verb) drawn from these FROZEN
// curated lists — no hash suffix, no harness prefix, no checksum word.
// A deterministic counter suffix arbitrates mint collisions.
//
// Curation rules (all pinned in phrase_test.go): lowercase ASCII
// letters only, at most 9 characters — short, common, phonetically
// distinct words, because the format's whole point is LLM
// transcription reliability; the three lists are pairwise disjoint;
// and the lists EXCLUDE the harness's observed adjective/noun slug
// vocabulary, so a session phrase never reads as an opencode slug
// (the shape rule — 3 words vs the slug's 2 — is the primary
// separator; this is the belt to its braces).
//
// Sizes: 160 x 192 x 64 = 1,966,080 phrases per workspace (~21 bits).
// Enumeration is ruled a non-issue (#1756: ids are references under
// authz, never capabilities); per-workspace uniqueness is arbitrated
// by the (workspace_id, short_id) index plus the counter suffix.
//
// FROZEN: sizes and membership are pinned by TestWordlists_Frozen;
// extending a list is a deliberate, reviewed change that updates the
// pin — never silent, never mid-release.

func wordSet(words []string) map[string]bool {
	set := make(map[string]bool, len(words))
	for _, w := range words {
		set[w] = true
	}
	return set
}

var (
	// Adjectives is the frozen adjective list (slot 1).
	Adjectives = wordSet([]string{
		"able", "acidic", "alert", "amber", "arctic", "atomic",
		"azure", "basic", "blunt", "bronze", "burly", "candid",
		"chalky", "chilled", "claret", "cloven", "copper", "cozy",
		"crimson", "crystal", "dapper", "daring", "dawning", "distant",
		"drafty", "dual", "early", "ebony", "elder", "electric",
		"ember", "ethereal", "fabled", "fallow", "feral", "fierce",
		"florid", "fossil", "fragrant", "fuzzy", "gilded", "glassy",
		"golden", "granite", "gusty", "handsome", "hazel", "heavy",
		"hilly", "inky", "insular", "ivory", "jasper", "junior",
		"lacy", "lean", "leisure", "level", "linear", "lofty",
		"lunar", "marble", "marshy", "meek", "merry", "mild",
		"misty", "modest", "mossy", "native", "noble", "novel",
		"obscure", "olive", "open", "orient", "outland", "pallid",
		"peaceful", "petite", "pitchy", "polished", "primal", "pristine",
		"profane", "profound", "punctual", "quaint", "quixotic", "rainy",
		"rational", "redolent", "regal", "remote", "rested", "retro",
		"rich", "rival", "robust", "rounder", "rugged", "sacred",
		"salted", "sappy", "savvy", "scenic", "secluded", "senior",
		"shaded", "silver", "skilled", "slick", "smooth", "sober",
		"solvent", "sought", "spatial", "spherical", "spurious", "stately",
		"suave", "subtle", "superb", "surging", "swarthy", "symbolic",
		"tangible", "teal", "tender", "thematic", "tidal", "tinted",
		"tonal", "torrid", "tranquil", "tropical", "turbid", "ultra",
		"upland", "usable", "valiant", "valued", "vanilla", "velvet",
		"verdant", "vibrant", "viridian", "visioned", "vivid", "warped",
		"wayward", "weathered", "western", "windy", "wise", "wooden",
		"worldly", "wrinkled", "yeasty", "zesty",
	})
	// Nouns is the frozen noun list (slot 2).
	Nouns = wordSet([]string{
		"abbey", "almanac", "archer", "auction", "balcony", "barn",
		"beacon", "bindery", "bloom", "border", "branch", "brochure",
		"bunker", "cadet", "canal", "carousel", "cathedral", "chalk",
		"chevron", "cipher", "cliff", "coach", "column", "condor",
		"corridor", "council", "crayon", "crown", "cyclone", "deckhand",
		"diagram", "dinner", "drifter", "dune", "echo", "elephant",
		"engine", "estate", "factory", "finch", "flatboat", "fountain",
		"frontier", "garden", "geyser", "glacier", "gondola", "granary",
		"guitar", "hammer", "harvest", "hawthorn", "henna", "honeycomb",
		"hurdle", "igloo", "inn", "ironwood", "javelin", "jockey",
		"juggler", "kelp", "keystone", "kitchen", "lantern", "larkspur",
		"lavender", "leopard", "lilac", "lobster", "loggia", "lumber",
		"magnet", "manger", "marigold", "marsh", "melon", "milestone",
		"mint", "monastery", "moonlight", "mosaic", "musket", "myriad",
		"nectar", "nickel", "noodle", "nutmeg", "octave", "okra",
		"orange", "oregano", "outpost", "oxbow", "pagoda", "pamphlet",
		"papaya", "parsley", "patio", "pelican", "persimmon", "pheasant",
		"piano", "pillow", "pintail", "pivot", "plaza", "podium",
		"poncho", "porcupine", "potpourri", "prospect", "pulley", "quadrant",
		"radar", "railroad", "raspberry", "recital", "refectory", "reservoir",
		"rift", "ritual", "roast", "rodeo", "rosemary", "rudder",
		"sailboat", "sapling", "scallop", "scroll", "seasoning", "sequoia",
		"shanty", "shingle", "shovel", "signal", "sitar", "sleeper",
		"snorkel", "sodium", "songbird", "spangle", "spectacle", "spiral",
		"squeegee", "stallion", "steeple", "stockade", "stovepipe", "sturgeon",
		"sunrise", "sushi", "sweater", "tabby", "tanker", "teapot",
		"tenor", "textile", "thimble", "tiara", "titanium", "toolbar",
		"tornado", "tourist", "toybox", "tram", "tray", "trinket",
		"trout", "turbine", "tuxedo", "ulster", "uniform", "urn",
		"varnish", "vehicle", "verdict", "vestibule", "vignette", "viperfish",
		"vocalist", "wagon", "wardrobe", "wasabi", "welder", "whistle",
		"windmill", "wolfhound", "woodland", "yacht", "yoke", "zinc",
	})
	// Verbs is the frozen verb (third-person singular) list (slot 3).
	Verbs = wordSet([]string{
		"ambles", "blooms", "cheers", "covers", "dines", "drills",
		"endures", "exhales", "feels", "folds", "generates", "greets",
		"hauls", "hovers", "indicates", "invokes", "kicks", "leaves",
		"loses", "masks", "mines", "mows", "notices", "orders",
		"paints", "pearls", "picnics", "plumbs", "pricks", "prosper",
		"puzzles", "rambles", "recants", "refrains", "remembers", "reprints",
		"restricts", "reviews", "rockets", "rushes", "scores", "seizes",
		"shines", "sings", "slogs", "snorts", "speeds", "squeals",
		"stencils", "strolls", "supports", "sweeps", "tasks", "thirsts",
		"tints", "tramps", "triumphs", "types", "upholds", "verifies",
		"waits", "weeds", "wiggles", "wounds",
	})
)
