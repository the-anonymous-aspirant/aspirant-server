package data_functions

import (
	"encoding/json"
	"log"
	"os"
	"strings"
	"unicode/utf8"
)

type DictionaryEntry struct {
	Definition string `json:"definition"`
	Example    string `json:"example,omitempty"`
}

// dictionaries maps language codes to their word dictionaries
var dictionaries map[string]map[string]map[string][]DictionaryEntry

// supportedLanguages defines which languages to attempt loading
var supportedLanguages = []struct {
	Code string
	File string
}{
	{"en", "dictionary.json"},
	{"sv", "dictionary_sv.json"},
	{"pt", "dictionary_pt.json"},
}

// LoadDictionary loads a dictionary for a specific language from a JSON file
func LoadDictionary(path string, lang string) error {
	byteValue, err := os.ReadFile(path)
	if err != nil {
		return err
	}

	var words map[string]map[string][]DictionaryEntry
	err = json.Unmarshal(byteValue, &words)
	if err != nil {
		return err
	}

	dictionaries[lang] = words
	return nil
}

// resolveLang returns the language to use, defaulting to "en"
func resolveLang(lang string) string {
	if lang == "" {
		return "en"
	}
	return lang
}

// IsWordInDictionary checks if a word is in the dictionary for the given language
func IsWordInDictionary(word string, lang string) bool {
	lang = resolveLang(lang)
	dict, exists := dictionaries[lang]
	if !exists {
		return false
	}
	trimmedWord := strings.TrimSpace(strings.ToLower(word))
	_, exists = dict[trimmedWord]
	return exists
}

func init() {
	dictionaries = make(map[string]map[string]map[string][]DictionaryEntry)

	basePath := os.Getenv("ASSET_BASE_PATH")
	if basePath == "" {
		basePath = "/data/assets"
	}

	for _, lang := range supportedLanguages {
		dictPath := basePath + "/games/" + lang.File
		err := LoadDictionary(dictPath, lang.Code)
		if err != nil {
			if lang.Code == "en" {
				log.Printf("Warning: Failed to load %s dictionary from %s: %v", lang.Code, dictPath, err)
				log.Println("Word validation features will not be available. The application will continue to run without dictionary functionality.")
			} else {
				log.Printf("Info: %s dictionary not found at %s (optional)", lang.Code, dictPath)
			}
		} else {
			log.Printf("Successfully loaded %s dictionary (%d words) from %s", lang.Code, len(dictionaries[lang.Code]), dictPath)
		}
	}
}

// GetRowSubstrings returns all substrings from a row in the board, then trims, then remove duplicates
func GetRowSubstrings(row []string) []string {
	substringSet := make(map[string]struct{})
	for i := 0; i < len(row); i++ {
		for j := i + 1; j <= len(row); j++ {
			substring := ""
			for k := i; k < j; k++ {
				substring += row[k]
			}
			trimmedSubstring := strings.TrimSpace(substring)
			if trimmedSubstring != "" {
				substringSet[trimmedSubstring] = struct{}{}
			}
		}
	}

	var substrings []string
	for substring := range substringSet {
		substrings = append(substrings, substring)
	}
	return substrings
}

// GetLongestValidWord returns the longest valid word from a list of words
func GetLongestValidWord(words []string, lang string) string {
	longestWord := ""
	longestRuneCount := 0
	for _, word := range words {
		runeCount := utf8.RuneCountInString(word)
		if runeCount > longestRuneCount && IsWordInDictionary(word, lang) {
			longestWord = word
			longestRuneCount = runeCount
		}
	}
	return longestWord
}

// GetLongestWordsFromBoard returns the longest word for each row and column from the board
func GetLongestWordsFromBoard(board [][]string, lang string) ([]string, []string) {
	longestWordsInRows := make([]string, len(board))
	longestWordsInCols := make([]string, len(board[0]))

	// Process rows
	for i, row := range board {
		substrings := GetRowSubstrings(row)
		longestWordsInRows[i] = GetLongestValidWord(substrings, lang)
	}

	// Process columns
	for j := 0; j < len(board[0]); j++ {
		var col []string
		for i := 0; i < len(board); i++ {
			col = append(col, board[i][j])
		}
		substrings := GetRowSubstrings(col)
		longestWordsInCols[j] = GetLongestValidWord(substrings, lang)
	}

	return longestWordsInRows, longestWordsInCols
}

// GetLongestWordsWithDefinitionsFromBoard returns the longest word and its definition for each row and column from the board
func GetLongestWordsWithDefinitionsFromBoard(board [][]string, lang string) ([]string, []string, []string, []string) {
	longestWordsInRows, longestWordsInCols := GetLongestWordsFromBoard(board, lang)

	rowDefinitions := make([]string, len(longestWordsInRows))
	colDefinitions := make([]string, len(longestWordsInCols))

	for i, word := range longestWordsInRows {
		if definition, exists := GetWordDefinition(word, lang); exists {
			rowDefinitions[i] = definition
		} else {
			rowDefinitions[i] = ""
		}
	}

	for i, word := range longestWordsInCols {
		if definition, exists := GetWordDefinition(word, lang); exists {
			colDefinitions[i] = definition
		} else {
			colDefinitions[i] = ""
		}
	}

	return longestWordsInRows, rowDefinitions, longestWordsInCols, colDefinitions
}

// GetWordDefinition returns the definition of a word from the dictionary
func GetWordDefinition(word string, lang string) (string, bool) {
	lang = resolveLang(lang)
	dict, exists := dictionaries[lang]
	if !exists {
		return "", false
	}

	trimmedWord := strings.TrimSpace(strings.ToLower(word))
	definitions, exists := dict[trimmedWord]
	if !exists {
		return "", false
	}

	var combinedDefinitions []string
	for partOfSpeech, defs := range definitions {
		for _, def := range defs {
			combinedDefinitions = append(combinedDefinitions, partOfSpeech+": "+def.Definition)
			if def.Example != "" {
				combinedDefinitions = append(combinedDefinitions, "Example: "+def.Example)
			}
		}
	}
	return strings.Join(combinedDefinitions, "; "), true
}
