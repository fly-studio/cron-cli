package main

func truncateText(s string, max int) string {
	if max >= len(s) {
		return s
	}
	return s[:max] + "..."
}
