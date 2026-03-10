package data_functions

import (
	"encoding/json"
	"log"
	"os"
	"strings"
)

// dictionary is a map of words to their parts of speech and definitions
var dictionary map[string]map[string][]struct {
	Definition string `json:"definition"`
	Example    string `json:"example,omitempty"`
}

// LoadDictionaryFromS3 loads the dictionary from a JSON file in an S3 bucket
func LoadDictionaryFromS3(bucket, key string) error {
	sess, err := InitS3Session()
	if err != nil {
		return err
	}

	byteValue, err := FetchFileFromS3(sess, bucket, key)
	if err != nil {
		return err
	}

	var words map[string]map[string][]struct {
		Definition string `json:"definition"`
		Example    string `json:"example,omitempty"`
	}
	err = json.Unmarshal(byteValue, &words)
	if err != nil {
		return err
	}

	dictionary = words

	return nil
}

// IsWordInDictionary checks if a word is in the dictionary
func IsWordInDictionary(word string) bool {
	trimmedWord := strings.TrimSpace(strings.ToLower(word))
	_, exists := dictionary[trimmedWord]
	return exists
}

func init() {
	bucket := os.Getenv("S3_BUCKET_NAME")
	key := "aspirant-website/games/wordweaver/eng_dict.json"
	err := LoadDictionaryFromS3(bucket, key)
	if err != nil {
		log.Printf("Warning: Failed to load dictionary from S3 bucket '%s' and key '%s': %v", bucket, key, err)
		log.Println("Word validation features will not be available. The application will continue to run without dictionary functionality.")
		// Initialize an empty dictionary to prevent nil pointer issues
		dictionary = make(map[string]map[string][]struct {
			Definition string `json:"definition"`
			Example    string `json:"example,omitempty"`
		})
	} else {
		log.Printf("Successfully loaded dictionary from S3 bucket '%s'", bucket)
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
func GetLongestValidWord(words []string) string {
	longestWord := ""
	for _, word := range words {
		if len(word) > len(longestWord) && IsWordInDictionary(word) {
			longestWord = word
		}
	}
	return longestWord
}

// GetLongestWordsFromBoard returns the longest word for each row and column from the board
func GetLongestWordsFromBoard(board [][]string) ([]string, []string) {
	longestWordsInRows := make([]string, len(board))
	longestWordsInCols := make([]string, len(board[0]))

	// Process rows
	for i, row := range board {
		substrings := GetRowSubstrings(row)
		longestWordsInRows[i] = GetLongestValidWord(substrings)
	}

	// Process columns
	for j := 0; j < len(board[0]); j++ {
		var col []string
		for i := 0; i < len(board); i++ {
			col = append(col, board[i][j])
		}
		substrings := GetRowSubstrings(col)
		longestWordsInCols[j] = GetLongestValidWord(substrings)
	}

	return longestWordsInRows, longestWordsInCols
}

// GetLongestWordsWithDefinitionsFromBoard returns the longest word and its definition for each row and column from the board
func GetLongestWordsWithDefinitionsFromBoard(board [][]string) ([]string, []string, []string, []string) {
	longestWordsInRows, longestWordsInCols := GetLongestWordsFromBoard(board)

	rowDefinitions := make([]string, len(longestWordsInRows))
	colDefinitions := make([]string, len(longestWordsInCols))

	for i, word := range longestWordsInRows {
		if definition, exists := GetWordDefinition(word); exists {
			rowDefinitions[i] = definition
		} else {
			rowDefinitions[i] = ""
		}
	}

	for i, word := range longestWordsInCols {
		if definition, exists := GetWordDefinition(word); exists {
			colDefinitions[i] = definition
		} else {
			colDefinitions[i] = ""
		}
	}

	return longestWordsInRows, rowDefinitions, longestWordsInCols, colDefinitions
}

// GetWordDefinition returns the definition of a word from the dictionary
func GetWordDefinition(word string) (string, bool) {
	trimmedWord := strings.TrimSpace(strings.ToLower(word))
	definitions, exists := dictionary[trimmedWord]
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
