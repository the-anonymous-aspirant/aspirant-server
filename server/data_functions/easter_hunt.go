package data_functions

import (
	"math/rand"
	"sync"
)

const (
	BoardWidth   = 128
	BoardHeight  = 128
	EggCount     = 24
	RevealRadius = 2 // ±2 cells = 5x5 area
)

// EggColors are pastel colors assigned to eggs round-robin.
var EggColors = []string{
	"#FF9AA2", "#FFB7B2", "#FFDAC1", "#E2F0CB",
	"#B5EAD7", "#C7CEEA", "#F0E6FF", "#FFE5B4",
}

// Point is a grid coordinate.
type Point struct {
	X int `json:"x"`
	Y int `json:"y"`
}

// Egg holds one egg's metadata and cell positions.
type Egg struct {
	ID      int     `json:"egg_id"`
	Color   string  `json:"color"`
	Squares []Point `json:"squares"`
}

// EggBoard holds the computed egg layout for one game seed.
type EggBoard struct {
	Eggs []Egg
	Grid [][]int // BoardWidth x BoardHeight, -1 = empty, 0..23 = egg ID
}

// eggTemplate defines the canonical egg shape as relative offsets.
// Approximately 100 cells arranged in an egg silhouette (tall oval,
// wider at top, narrower at bottom). 15 rows tall, up to 9 wide.
//
//	Row 0:        . . . . X X . . .   (2)
//	Row 1:        . . . X X X X . .   (4)
//	Row 2:        . . X X X X X X .   (6)
//	Row 3:        . X X X X X X X .   (7)
//	Row 4:        . X X X X X X X X   (8)
//	Row 5:        X X X X X X X X X   (9)
//	Row 6:        X X X X X X X X X   (9)
//	Row 7:        X X X X X X X X X   (9)
//	Row 8:        . X X X X X X X X   (8)
//	Row 9:        . X X X X X X X .   (7)
//	Row 10:       . . X X X X X X .   (6)
//	Row 11:       . . X X X X X . .   (5)
//	Row 12:       . . . X X X X . .   (4)
//	Row 13:       . . . . X X X . .   (3)
//	Row 14:       . . . . . X . . .   (1)
//
// Total: 88 cells
var eggTemplate []Point

func init() {
	// rows defines (startX, count) for each row of the egg shape
	rows := []struct{ start, count int }{
		{4, 2},  // row 0
		{3, 4},  // row 1
		{2, 6},  // row 2
		{1, 7},  // row 3
		{1, 8},  // row 4
		{0, 9},  // row 5
		{0, 9},  // row 6
		{0, 9},  // row 7
		{1, 8},  // row 8
		{1, 7},  // row 9
		{2, 6},  // row 10
		{2, 5},  // row 11
		{3, 4},  // row 12
		{4, 3},  // row 13
		{5, 1},  // row 14
	}
	for y, row := range rows {
		for dx := 0; dx < row.count; dx++ {
			eggTemplate = append(eggTemplate, Point{X: row.start + dx, Y: y})
		}
	}
}

// EggSize returns the number of cells per egg (template size).
func EggSize() int {
	return len(eggTemplate)
}

// templateWidth and templateHeight for the canonical orientation.
const templateWidth = 9
const templateHeight = 15

// rotateTemplate returns the egg template rotated by 0, 90, 180, or 270 degrees.
// Returns the rotated offsets and the bounding width/height.
func rotateTemplate(rotation int) ([]Point, int, int) {
	switch rotation {
	case 0:
		out := make([]Point, len(eggTemplate))
		copy(out, eggTemplate)
		return out, templateWidth, templateHeight
	case 1: // 90° CW: (x,y) → (maxY-y, x)
		out := make([]Point, len(eggTemplate))
		for i, p := range eggTemplate {
			out[i] = Point{X: (templateHeight - 1) - p.Y, Y: p.X}
		}
		return out, templateHeight, templateWidth
	case 2: // 180°: (x,y) → (maxX-x, maxY-y)
		out := make([]Point, len(eggTemplate))
		for i, p := range eggTemplate {
			out[i] = Point{X: (templateWidth - 1) - p.X, Y: (templateHeight - 1) - p.Y}
		}
		return out, templateWidth, templateHeight
	case 3: // 270° CW: (x,y) → (y, maxX-x)
		out := make([]Point, len(eggTemplate))
		for i, p := range eggTemplate {
			out[i] = Point{X: p.Y, Y: (templateWidth - 1) - p.X}
		}
		return out, templateHeight, templateWidth
	}
	panic("invalid rotation")
}

// boardCache stores computed boards keyed by seed.
var boardCache sync.Map

// GenerateEggBoard computes the egg layout for a given seed.
// Results are cached — repeated calls with the same seed return the same board.
func GenerateEggBoard(seed int64) *EggBoard {
	if cached, ok := boardCache.Load(seed); ok {
		return cached.(*EggBoard)
	}
	board := computeEggBoard(seed)
	boardCache.Store(seed, board)
	return board
}

// ClearBoardCache removes a seed from the cache (used on game reset).
func ClearBoardCache(seed int64) {
	boardCache.Delete(seed)
}

func newGrid() [][]int {
	grid := make([][]int, BoardWidth)
	for x := 0; x < BoardWidth; x++ {
		grid[x] = make([]int, BoardHeight)
		for y := 0; y < BoardHeight; y++ {
			grid[x][y] = -1
		}
	}
	return grid
}

func computeEggBoard(seed int64) *EggBoard {
	rng := rand.New(rand.NewSource(seed))

	board := &EggBoard{Grid: newGrid()}

	colorOrder := rng.Perm(len(EggColors))

	for eggID := 0; eggID < EggCount; eggID++ {
		rotation := rng.Intn(4)
		offsets, w, h := rotateTemplate(rotation)
		color := EggColors[colorOrder[eggID%len(EggColors)]]

		placed := false
		for attempt := 0; attempt < 500; attempt++ {
			ax := rng.Intn(BoardWidth - w + 1)
			ay := rng.Intn(BoardHeight - h + 1)

			if canPlace(board, offsets, ax, ay) {
				squares := placeEgg(board, offsets, ax, ay, eggID)
				board.Eggs = append(board.Eggs, Egg{
					ID:      eggID,
					Color:   color,
					Squares: squares,
				})
				placed = true
				break
			}
		}

		if !placed {
			// Retry with shifted seed
			return computeEggBoard(seed + 1)
		}
	}

	return board
}

func canPlace(board *EggBoard, offsets []Point, ax, ay int) bool {
	for _, off := range offsets {
		x, y := ax+off.X, ay+off.Y
		if board.Grid[x][y] != -1 {
			return false
		}
	}
	return true
}

func placeEgg(board *EggBoard, offsets []Point, ax, ay, eggID int) []Point {
	squares := make([]Point, 0, len(offsets))
	for _, off := range offsets {
		x, y := ax+off.X, ay+off.Y
		board.Grid[x][y] = eggID
		squares = append(squares, Point{X: x, Y: y})
	}
	return squares
}
