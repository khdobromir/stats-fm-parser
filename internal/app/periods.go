package app

import "stats-fm-parser/internal/statsfm"

var periodChoices = [...]struct{ key, label string }{
	{"", "Все стандартные"},
	{"week", "За неделю"},
	{"weeks", "За 4 недели"},
	{"months", "За 6 месяцев"},
	{"ytd", "За текущий год"},
	{statsfm.CustomPeriod, "Свои даты"},
}
