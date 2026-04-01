package data_functions

import (
	"testing"
)

func TestGenerateEggBoard_Deterministic(t *testing.T) {
	seed := int64(42)
	ClearBoardCache(seed)
	board1 := GenerateEggBoard(seed)
	ClearBoardCache(seed)
	board2 := GenerateEggBoard(seed)

	if len(board1.Eggs) != len(board2.Eggs) {
		t.Fatalf("egg count mismatch: %d vs %d", len(board1.Eggs), len(board2.Eggs))
	}

	for i := range board1.Eggs {
		if board1.Eggs[i].ID != board2.Eggs[i].ID {
			t.Errorf("egg %d: ID mismatch", i)
		}
		if board1.Eggs[i].Color != board2.Eggs[i].Color {
			t.Errorf("egg %d: color mismatch", i)
		}
		for j := range board1.Eggs[i].Squares {
			if board1.Eggs[i].Squares[j] != board2.Eggs[i].Squares[j] {
				t.Errorf("egg %d square %d: position mismatch", i, j)
			}
		}
	}

	for x := 0; x < BoardWidth; x++ {
		for y := 0; y < BoardHeight; y++ {
			if board1.Grid[x][y] != board2.Grid[x][y] {
				t.Errorf("grid mismatch at (%d,%d): %d vs %d", x, y, board1.Grid[x][y], board2.Grid[x][y])
			}
		}
	}
}

func TestGenerateEggBoard_CorrectCounts(t *testing.T) {
	seed := int64(12345)
	ClearBoardCache(seed)
	board := GenerateEggBoard(seed)

	if len(board.Eggs) != EggCount {
		t.Fatalf("expected %d eggs, got %d", EggCount, len(board.Eggs))
	}

	eggSize := EggSize()
	for i, egg := range board.Eggs {
		if len(egg.Squares) != eggSize {
			t.Errorf("egg %d: expected %d squares, got %d", i, eggSize, len(egg.Squares))
		}
	}

	occupiedCount := 0
	for x := 0; x < BoardWidth; x++ {
		for y := 0; y < BoardHeight; y++ {
			if board.Grid[x][y] != -1 {
				occupiedCount++
			}
		}
	}
	expected := EggCount * eggSize
	if occupiedCount != expected {
		t.Errorf("expected %d occupied squares, got %d", expected, occupiedCount)
	}
}

func TestGenerateEggBoard_NoOverlap(t *testing.T) {
	seed := int64(99999)
	ClearBoardCache(seed)
	board := GenerateEggBoard(seed)

	seen := make(map[Point]int)
	for _, egg := range board.Eggs {
		for _, sq := range egg.Squares {
			if prevEgg, exists := seen[sq]; exists {
				t.Errorf("overlap at (%d,%d): egg %d and egg %d", sq.X, sq.Y, prevEgg, egg.ID)
			}
			seen[sq] = egg.ID
		}
	}
}

func TestGenerateEggBoard_DifferentSeeds(t *testing.T) {
	ClearBoardCache(1)
	ClearBoardCache(2)
	board1 := GenerateEggBoard(1)
	board2 := GenerateEggBoard(2)

	different := false
	for x := 0; x < BoardWidth; x++ {
		for y := 0; y < BoardHeight; y++ {
			if board1.Grid[x][y] != board2.Grid[x][y] {
				different = true
				break
			}
		}
		if different {
			break
		}
	}

	if !different {
		t.Error("different seeds produced identical boards")
	}
}

func TestGenerateEggBoard_BoundsValid(t *testing.T) {
	seed := int64(77777)
	ClearBoardCache(seed)
	board := GenerateEggBoard(seed)

	for _, egg := range board.Eggs {
		for _, sq := range egg.Squares {
			if sq.X < 0 || sq.X >= BoardWidth || sq.Y < 0 || sq.Y >= BoardHeight {
				t.Errorf("egg %d: square (%d,%d) out of bounds", egg.ID, sq.X, sq.Y)
			}
		}
	}
}

func TestGenerateEggBoard_EggColors(t *testing.T) {
	seed := int64(555)
	ClearBoardCache(seed)
	board := GenerateEggBoard(seed)

	for _, egg := range board.Eggs {
		if egg.Color == "" {
			t.Errorf("egg %d has empty color", egg.ID)
		}
		found := false
		for _, c := range EggColors {
			if egg.Color == c {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("egg %d has invalid color %s", egg.ID, egg.Color)
		}
	}
}

func TestEggTemplate_Size(t *testing.T) {
	size := EggSize()
	if size < 80 || size > 120 {
		t.Errorf("egg template size %d is outside expected range [80, 120]", size)
	}
}

func TestRotateTemplate_PreservesSize(t *testing.T) {
	baseSize := EggSize()
	for rot := 0; rot < 4; rot++ {
		offsets, _, _ := rotateTemplate(rot)
		if len(offsets) != baseSize {
			t.Errorf("rotation %d: expected %d cells, got %d", rot, baseSize, len(offsets))
		}
	}
}

func TestRotateTemplate_NoDuplicates(t *testing.T) {
	for rot := 0; rot < 4; rot++ {
		offsets, _, _ := rotateTemplate(rot)
		seen := make(map[Point]bool)
		for _, p := range offsets {
			if seen[p] {
				t.Errorf("rotation %d: duplicate point (%d,%d)", rot, p.X, p.Y)
			}
			seen[p] = true
		}
	}
}
