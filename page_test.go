package rod_test

func (s *S) TestClosePage() {
	page := s.browser.Page("https://google.com")
	page.Element("input")
	page.Close()
}

func (s *S) TestPage() {
	s.page.Navigate(s.htmlFile("click.html"))

	s.Equal("<h4>Title</h4>", s.page.Element("h4").HTML())
}
