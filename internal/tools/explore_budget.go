package tools

// ExploreOutputBudget is the adaptive explore output ceiling, scaled to project size.
// Larger tiers must never get a smaller per-file or total cap than smaller tiers
// (monotonic; mirrors official getExploreOutputBudget).
type ExploreOutputBudget struct {
	MaxOutputChars              int
	DefaultMaxFiles             int
	MaxCharsPerFile             int
	MaxSymbolsInFileHeader      int
	MaxEdgesPerRelationshipKind int
	IncludeRelationships        bool
	IncludeAdditionalFiles      bool
	IncludeCompletenessSignal   bool
	IncludeBudgetNote           bool
	ExcludeLowValueFiles        bool
}

// GetExploreBudget returns the recommended number of explore calls for a project size.
func GetExploreBudget(fileCount int) int {
	switch {
	case fileCount < 500:
		return 1
	case fileCount < 5000:
		return 2
	case fileCount < 15000:
		return 3
	case fileCount < 25000:
		return 4
	default:
		return 5
	}
}

// GetExploreOutputBudget returns per-tier output ceilings. Invariant: a larger
// tier never gets a smaller MaxCharsPerFile or MaxOutputChars than a smaller one.
func GetExploreOutputBudget(fileCount int) ExploreOutputBudget {
	switch {
	case fileCount < 150:
		return ExploreOutputBudget{
			MaxOutputChars:              13000,
			DefaultMaxFiles:             4,
			MaxCharsPerFile:             3800,
			MaxSymbolsInFileHeader:      5,
			MaxEdgesPerRelationshipKind: 4,
			IncludeRelationships:        false,
			IncludeAdditionalFiles:      false,
			IncludeCompletenessSignal:   false,
			IncludeBudgetNote:           false,
			ExcludeLowValueFiles:        true,
		}
	case fileCount < 500:
		return ExploreOutputBudget{
			MaxOutputChars:              18000,
			DefaultMaxFiles:             5,
			MaxCharsPerFile:             3800,
			MaxSymbolsInFileHeader:      6,
			MaxEdgesPerRelationshipKind: 6,
			IncludeRelationships:        false,
			IncludeAdditionalFiles:      false,
			IncludeCompletenessSignal:   false,
			IncludeBudgetNote:           false,
			ExcludeLowValueFiles:        true,
		}
	case fileCount < 5000:
		return ExploreOutputBudget{
			MaxOutputChars:              24000,
			DefaultMaxFiles:             8,
			MaxCharsPerFile:             6500,
			MaxSymbolsInFileHeader:      10,
			MaxEdgesPerRelationshipKind: 10,
			IncludeRelationships:        true,
			IncludeAdditionalFiles:      true,
			IncludeCompletenessSignal:   true,
			IncludeBudgetNote:           true,
			ExcludeLowValueFiles:        false,
		}
	case fileCount < 15000:
		return ExploreOutputBudget{
			MaxOutputChars:              24000,
			DefaultMaxFiles:             8,
			MaxCharsPerFile:             7000,
			MaxSymbolsInFileHeader:      15,
			MaxEdgesPerRelationshipKind: 15,
			IncludeRelationships:        true,
			IncludeAdditionalFiles:      true,
			IncludeCompletenessSignal:   true,
			IncludeBudgetNote:           true,
			ExcludeLowValueFiles:        false,
		}
	default:
		return ExploreOutputBudget{
			MaxOutputChars:              24000,
			DefaultMaxFiles:             8,
			MaxCharsPerFile:             7000,
			MaxSymbolsInFileHeader:      15,
			MaxEdgesPerRelationshipKind: 15,
			IncludeRelationships:        true,
			IncludeAdditionalFiles:      true,
			IncludeCompletenessSignal:   true,
			IncludeBudgetNote:           true,
			ExcludeLowValueFiles:        false,
		}
	}
}
