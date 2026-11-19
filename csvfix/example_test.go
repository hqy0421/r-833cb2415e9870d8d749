package csvfix_test

import (
	"fmt"

	"github.com/gocolly/colly/v2/csvfix"
)

// A two-column export whose second row lost its separator. With symmetric
// unit costs there are two equally cheap repairs decoding to different
// records, so Repair reports ambiguity with two witnesses instead of
// guessing. With costs that make the comma-split strictly cheaper, the same
// input has a single minimum-cost repair.
func ExampleRepair_ambiguity() {
	unit := csvfix.Costs{
		InsertComma: 1, DeleteComma: 1,
		InsertQuote: 1, DeleteQuote: 1,
		InsertLF: 1, DeleteLF: 1,
	}
	res, err := csvfix.Repair("name,age\nAda36\n", 2, unit)
	if err != nil {
		fmt.Println("error:", err)
		return
	}
	if res.Ambiguous {
		fmt.Println("ambiguous, minimum cost:", res.Cost)
		for _, w := range res.Witnesses {
			fmt.Printf("witness %q -> %v\n", w.Repaired, w.Records)
		}
	}
	// Output:
	// ambiguous, minimum cost: 1
	// witness "name,age\nAda36,\n" -> [[name age] [Ada36 ]]
	// witness "name,age\nAda3,6\n" -> [[name age] [Ada3 6]]
}

func ExampleRepair_unique() {
	// One column: a field missing only its trailing LF has exactly one
	// minimum-cost repair.
	unit := csvfix.Costs{
		InsertComma: 1, DeleteComma: 1,
		InsertQuote: 1, DeleteQuote: 1,
		InsertLF: 1, DeleteLF: 1,
	}
	res, err := csvfix.Repair("ab", 1, unit)
	if err != nil {
		fmt.Println("error:", err)
		return
	}
	fmt.Println("cost:", res.Cost)
	fmt.Println("edits:", res.Edits)
	fmt.Printf("repaired: %q\n", res.Repaired)
	fmt.Println("records:", res.Records)
	// Output:
	// cost: 1
	// edits: [insert '\n' at 2]
	// repaired: "ab\n"
	// records: [[ab]]
}
