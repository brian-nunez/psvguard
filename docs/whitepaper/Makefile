# White paper build. The figure data comes from: go run -tags whitepaper ./docs/whitepaper/figdata
PDF := psvguard-whitepaper.pdf

$(PDF): main.tex data/measured.tex $(wildcard data/*.csv) $(wildcard figures/*.png figures/generated/*.png)
	latexmk -pdf -interaction=nonstopmode -halt-on-error -output-directory=build main.tex
	cp build/main.pdf $(PDF)

.PHONY: clean
clean:
	latexmk -C -output-directory=build main.tex
