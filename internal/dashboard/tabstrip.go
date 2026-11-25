package dashboard

import "github.com/egoist/mygo/ui"

// tabStrip is a strip of tabs over what they choose among: the Browser
// Tabs of a browser that has more than one, over the Viewport that shows
// one of them. It takes another line where its tabs do not fit in one.
// label says what the strip is of, for whoever does not see where it
// stands.
func tabStrip(c *ui.Context, label string, selected *int, tabs []string) {
	t := c.Theme()
	ui.Row(c).Shrink(0).FillWidth().MinHeight(stripHeight).AlignItems(ui.Stretch).Font(chromeFont).Label(label).
		Background(panelTheme.Background).BorderWidth(0, 0, 1, 0).BorderColor(t.Border).Children(func() {
		parts := ui.TabsBase(c, selected, len(tabs))
		parts.List.Row().Wrap().Grow(1).Shrink(1).Children(func() {
			for i, one := range tabs {
				e := parts.Tab(i).Row().Gap(t.Space(2)).Padding(0, t.Space(3)).MinHeight(stripHeight).AlignItems(ui.Center).
					BorderWidth(0, 1, 0, 0).BorderColor(t.Border)
				chosen := i == *selected
				if chosen {
					e.Background(t.Background)
				}
				e.Children(func() {
					color := t.TextMuted
					if chosen {
						color = t.Text
					}
					ui.Text(c, one).MaxLines(1).TextColor(color)
				})
			}
		})
	})
}
