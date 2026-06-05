package catalog

func defaultChannels() []string {
	return []string{
		"llm_notification",
		"webhook",
		"log",
		"slack",
		"gmail",
		"email",
		"telegram",
		"discord",
		"voice_twilio",
		"voice_websocket",
		"kafka",
		"sms",
		"http",
		"queue",
		"rss",
		"sse",
	}
}
