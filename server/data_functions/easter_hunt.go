package data_functions

import (
	"math/rand"
	"sync"
)

const (
	BoardWidth  = 32
	BoardHeight = 32
	EggCount    = 24
	EggSize     = 16
)

// EggColors are pastel colors assigned to eggs round-robin.
var EggColors = []string{
	"#FF9AA2", "#FFB7B2", "#FFDAC1", "#E2F0CB",
	"#B5EAD7", "#C7CEEA", "#F0E6FF", "#FFE5B4",
}

// EggShape defines relative (dx, dy) offsets for one egg variant.
type EggShape struct {
	Offsets []Point
	Width   int
	Height  int
}

type Point struct {
	X int `json:"x"`
	Y int `json:"y"`
}

type Egg struct {
	ID      int     `json:"egg_id"`
	Color   string  `json:"color"`
	Squares []Point `json:"squares"`
}

// EggBoard holds the computed egg layout for one game seed.
type EggBoard struct {
	Eggs    []Egg
	Grid    [BoardWidth][BoardHeight]int // -1 = empty, 0..23 = egg ID
}

// Three egg shape variants — each has 16 cells.
//
// Variant A (tall, 4 wide x 5 tall):
//
//	. X X .
//	X X X X
//	X X X X
//	X X X X
//	. X X .
var shapeA = EggShape{
	Offsets: []Point{
		{1, 0}, {2, 0},
		{0, 1}, {1, 1}, {2, 1}, {3, 1},
		{0, 2}, {1, 2}, {2, 2}, {3, 2},
		{0, 3}, {1, 3}, {2, 3}, {3, 3},
		{1, 4}, {2, 4},
	},
	Width: 4, Height: 5,
}

// Variant B (wide, 6 wide x 4 tall):
//
//	. . X X . .
//	. X X X X .
//	X X X X X X
//	. X X X X .
var shapeB = EggShape{
	Offsets: []Point{
		{2, 0}, {3, 0},
		{1, 1}, {2, 1}, {3, 1}, {4, 1},
		{0, 2}, {1, 2}, {2, 2}, {3, 2}, {4, 2}, {5, 2},
		{1, 3}, {2, 3}, {3, 3}, {4, 3},
	},
	Width: 6, Height: 4,
}

// Variant C (round, 4 wide x 5 tall):
//
//	. X X .
//	X X X X
//	X X X X
//	X X X X
//	. X X .
//
// Same silhouette as A but shifted weight — the top is narrower
// giving a slightly different egg personality when rotated by
// the random placement.
var shapeC = EggShape{
	Offsets: []Point{
		{1, 0}, {2, 0},
		{0, 1}, {1, 1}, {2, 1}, {3, 1},
		{0, 2}, {1, 2}, {2, 2}, {3, 2},
		{0, 3}, {1, 3}, {2, 3}, {3, 3},
		{1, 4}, {2, 4},
	},
	Width: 4, Height: 5,
}

var shapes = []EggShape{shapeA, shapeB, shapeC}

// boardCache stores computed boards keyed by seed.
var boardCache sync.Map

func init() {
	// Verify each shape has exactly EggSize offsets.
	for _, s := range shapes {
		if len(s.Offsets) != EggSize {
			panic("easter_hunt: shape variant has wrong number of offsets")
		}
	}
}

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

func computeEggBoard(seed int64) *EggBoard {
	rng := rand.New(rand.NewSource(seed))

	board := &EggBoard{}
	for x := 0; x < BoardWidth; x++ {
		for y := 0; y < BoardHeight; y++ {
			board.Grid[x][y] = -1
		}
	}

	// Shuffle color assignment
	colorOrder := rng.Perm(len(EggColors))

	for eggID := 0; eggID < EggCount; eggID++ {
		shape := shapes[rng.Intn(len(shapes))]
		color := EggColors[colorOrder[eggID%len(EggColors)]]

		placed := false
		for attempt := 0; attempt < 200; attempt++ {
			ax := rng.Intn(BoardWidth - shape.Width + 1)
			ay := rng.Intn(BoardHeight - shape.Height + 1)

			if canPlace(board, shape, ax, ay) {
				squares := placeEgg(board, shape, ax, ay, eggID)
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
			// Extremely unlikely with 24 eggs on a 32x32 board.
			// Retry the whole board with a shifted seed.
			return computeEggBoard(seed + 1)
		}
	}

	return board
}

func canPlace(board *EggBoard, shape EggShape, ax, ay int) bool {
	for _, off := range shape.Offsets {
		x, y := ax+off.X, ay+off.Y
		if board.Grid[x][y] != -1 {
			return false
		}
	}
	return true
}

func placeEgg(board *EggBoard, shape EggShape, ax, ay, eggID int) []Point {
	squares := make([]Point, 0, EggSize)
	for _, off := range shape.Offsets {
		x, y := ax+off.X, ay+off.Y
		board.Grid[x][y] = eggID
		squares = append(squares, Point{X: x, Y: y})
	}
	return squares
}
