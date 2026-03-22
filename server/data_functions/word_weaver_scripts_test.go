package data_functions

import (
	"testing"
)

func requireDictionary(t *testing.T) {
	t.Helper()
	if len(dictionary) == 0 {
		t.Skip("Skipping: dictionary not loaded (requires dictionary file at ASSET_BASE_PATH/games/dictionary.json)")
	}
}

func TestWordExistsInDictionary(t *testing.T) {
	requireDictionary(t)
	word := "abandons"
	if _, exists := dictionary[word]; !exists {
		t.Errorf("Expected word %s to be in the dictionary", word)
	}
}

func TestWordDoesNotExistInDictionary(t *testing.T) {
	requireDictionary(t)
	word := "nonexistentword"
	if _, exists := dictionary[word]; exists {
		t.Errorf("Did not expect word %s to be in the dictionary", word)
	}
}

func TestGetAllSubstrings(t *testing.T) {
	board := [][]string{
		{"c", "a", "t"},
		{"a", " ", "b"},
		{"t", " ", " "},
	}

	expectedRow1 := []string{"c", "ca", "cat", "a", "at", "t"}
	expectedRow2 := []string{"a", "a b", "b"}
	expectedRow3 := []string{"t"}

	expectedCol1 := []string{"c", "ca", "cat", "a", "at", "t"}
	expectedCol2 := []string{"a"}
	expectedCol3 := []string{"tb", "t", "b"}

	tests := []struct {
		row      []string
		expected []string
	}{
		{board[0], expectedRow1},
		{board[1], expectedRow2},
		{board[2], expectedRow3},
		{[]string{board[0][0], board[1][0], board[2][0]}, expectedCol1},
		{[]string{board[0][1], board[1][1], board[2][1]}, expectedCol2},
		{[]string{board[0][2], board[1][2], board[2][2]}, expectedCol3},
	}

	for i, test := range tests {
		substrings := GetRowSubstrings(test.row)
		if !equalUnordered(substrings, test.expected) {
			t.Errorf("Test case %d failed: expected %v, got %v", i+1, test.expected, substrings)
		}
	}
}

func equalUnordered(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	aMap := make(map[string]int)
	bMap := make(map[string]int)
	for _, v := range a {
		aMap[v]++
	}
	for _, v := range b {
		bMap[v]++
	}
	for k, v := range aMap {
		if bMap[k] != v {
			return false
		}
	}
	return true
}

func TestGetLongestValidWord(t *testing.T) {
	requireDictionary(t)
	words := []string{"cat", "caterpillar", "dog", "elephant"}
	expected := "caterpillar"

	longestWord := GetLongestValidWord(words)
	if longestWord != expected {
		t.Errorf("Expected longest valid word to be %s, but got %s", expected, longestWord)
	}
}

func TestGetLongestValidWordNoValidWords(t *testing.T) {
	requireDictionary(t)
	words := []string{"nonexistentword1", "nonexistentword2"}
	expected := ""

	longestWord := GetLongestValidWord(words)
	if longestWord != expected {
		t.Errorf("Expected longest valid word to be %s, but got %s", expected, longestWord)
	}
}

func TestGetLongestValidWordEmptyList(t *testing.T) {
	requireDictionary(t)
	words := []string{}
	expected := ""

	longestWord := GetLongestValidWord(words)
	if longestWord != expected {
		t.Errorf("Expected longest valid word to be %s, but got %s", expected, longestWord)
	}
}

func TestGetLongestWordsFromBoard(t *testing.T) {
	requireDictionary(t)
	board := [][]string{
		{"c", "a", "t"},
		{"a", " ", "b"},
		{"b", " ", " "},
	}

	expectedRows := []string{"cat", "", ""}
	expectedCols := []string{"cab", "", ""}

	longestWordsInRows, longestWordsInCols := GetLongestWordsFromBoard(board)

	if !equalUnordered(longestWordsInRows, expectedRows) {
		t.Errorf("Expected longest words in rows to be %v, but got %v", expectedRows, longestWordsInRows)
	}

	if !equalUnordered(longestWordsInCols, expectedCols) {
		t.Errorf("Expected longest words in columns to be %v, but got %v", expectedCols, longestWordsInCols)
	}
}

func TestGetLongestWordsFromBoardEmptyBoard(t *testing.T) {
	requireDictionary(t)
	board := [][]string{
		{"", "", ""},
		{"", "", ""},
		{"", "", ""},
	}

	expectedRows := []string{"", "", ""}
	expectedCols := []string{"", "", ""}

	longestWordsInRows, longestWordsInCols := GetLongestWordsFromBoard(board)

	if !equalUnordered(longestWordsInRows, expectedRows) {
		t.Errorf("Expected longest words in rows to be %v, but got %v", expectedRows, longestWordsInRows)
	}

	if !equalUnordered(longestWordsInCols, expectedCols) {
		t.Errorf("Expected longest words in columns to be %v, but got %v", expectedCols, longestWordsInCols)
	}
}

func TestGetLongestWordsFromBoardNoValidWords(t *testing.T) {
	requireDictionary(t)
	board := [][]string{
		{"y", "y", "y"},
		{"x", "x", "s"},
		{"x", "y", "z"},
	}

	expectedRows := []string{"", "", ""}
	expectedCols := []string{"", "", ""}

	longestWordsInRows, longestWordsInCols := GetLongestWordsFromBoard(board)

	if !equalUnordered(longestWordsInRows, expectedRows) {
		t.Errorf("Expected longest words in rows to be %v, but got %v", expectedRows, longestWordsInRows)
	}

	if !equalUnordered(longestWordsInCols, expectedCols) {
		t.Errorf("Expected longest words in columns to be %v, but got %v", expectedCols, longestWordsInCols)
	}
}
