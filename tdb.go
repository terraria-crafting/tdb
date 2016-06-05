package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"gopkg.in/inconshreveable/log15.v2"
)

type Dump struct {
	Items       []Item            `json:"items"`
	Recipes     []Recipe          `json:"recipes"`
	ItemIndex   map[string]Item   `json:"itemindex"`
	RecipeIndex map[string]Recipe `json:"recipeindex"`
	Relations   map[string][]int  `json:"relations"`
}

type Item struct {
	Id    int    `json:"id"`
	Name  string `json:"label"`
	Image string `json:"image"`
}

type Ingredient struct {
	Item  int `json:"item"`
	Count int `json:"count"`
}

type Workstation struct {
	Item  int    `json:"item"`
	Other string `json:"other"`
}

type Recipe struct {
	Id           int           `json:"id"`
	Workstations []Workstation `json:"workstations"`
	Ingredients  []Ingredient  `json:"ingredients"`
	Product      Ingredient    `json:"product"`
}

func main() {
	log15.Root().SetHandler(log15.LvlFilterHandler(log15.LvlDebug, log15.StderrHandler))

	// Download the data from the Terraria Wiki
	log15.Info("retrieving item list")
	itemlist := listItems()

	log15.Info("downloading item images")
	os.Mkdir("img", 0777)
	for i, item := range itemlist {
		path := filepath.Join("img", filepath.Base(item.Image))
		path = path[:strings.Index(path, "?")]

		// Only download the image if not yet cached
		if _, err := os.Stat(path); os.IsNotExist(err) {
			log15.Debug("downloading image", "index", i, "total", len(itemlist), "item", item.Name)
			res, _ := http.Get(item.Image)
			defer res.Body.Close()

			img, _ := ioutil.ReadAll(res.Body)
			ioutil.WriteFile(path, img, 0777)
		} else {
			log15.Debug("skipping cached image", "index", i, "total", len(itemlist), "item", item.Name)
		}
		itemlist[i].Image = path
	}
	log15.Info("retrieving recipe list")
	items, ids := make(map[int]Item), make(map[string]int)
	for _, item := range itemlist {
		items[item.Id], ids[item.Name] = item, item.Id
	}
	recipes := listRecipes(ids)

	// Filter out all danglig nodes of no interest
	log15.Info("indexing dataset")
	itemIndex, recipeIndex, relations := make(map[string]Item), make(map[string]Recipe), make(map[string][]int)
	for _, recipe := range recipes {
		itemIndex[strconv.Itoa(recipe.Product.Item)] = items[recipe.Product.Item]
		recipeIndex[strconv.Itoa(recipe.Id)] = recipe
		relations[strconv.Itoa(recipe.Product.Item)] = append(relations[strconv.Itoa(recipe.Product.Item)], recipe.Id)

		for _, ingredient := range recipe.Ingredients {
			itemIndex[strconv.Itoa(ingredient.Item)] = items[ingredient.Item]
			relations[strconv.Itoa(ingredient.Item)] = append(relations[strconv.Itoa(ingredient.Item)], recipe.Id)
		}
	}
	delete(itemIndex, "0")

	itemlist = []Item{}
	for _, item := range itemIndex {
		itemlist = append(itemlist, item)
	}
	// Dump the data into an importable json/JavaScript file
	out, _ := json.Marshal(Dump{Items: itemlist, Recipes: recipes, ItemIndex: itemIndex, RecipeIndex: recipeIndex, Relations: relations})
	ioutil.WriteFile("data.json", []byte(fmt.Sprintf("var data = %s;", out)), 0777)
}

// listItems retrieves a list of all known iterms form the Terraria Wiki.
func listItems() []Item {
	items := []Item{}
	for page := 1; page <= 13; page++ {
		log15.Debug("parsing item listing", "page", page)
		res, _ := http.Get(fmt.Sprintf("http://terraria.gamepedia.com/Item_IDs_Part%d", page))
		defer res.Body.Close()

		body := bufio.NewScanner(res.Body)
		for body.Scan() {
			// Skip any bullshit before the item listing
			line := body.Text()
			if !strings.Contains(line, "<span style=\"white-space:nowrap\">") {
				continue
			}
			// Strip out the item name and image
			name := strings.TrimSpace(regexp.MustCompile("title=\"([^\"]+)\"").FindStringSubmatch(line)[1])
			name = strings.Replace(name, "&#39;", "'", -1)

			image := strings.TrimSpace(regexp.MustCompile("src=\"([^\"]+)\"").FindStringSubmatch(line)[1])
			image = strings.Replace(image, "%27", "'", -1)

			// Strip out the item ID from the next line
			body.Scan()
			sid := strings.TrimSpace(regexp.MustCompile("[0-9]+").FindString(body.Text()))
			id, _ := strconv.Atoi(sid)

			items = append(items, Item{Id: id, Name: name, Image: image})
		}
	}
	return items
}

// listRecipes retrieves a list of all recipes from the Terraria Wiki. The item
// components are retrieved from a set of existing items.
func listRecipes(items map[string]int) []Recipe {
	recipes := []Recipe{}

	// Retrieve all the workstations to iterate the recipes for
	log15.Debug("parsing workstation listing")
	res, _ := http.Get("http://terraria.gamepedia.com/Recipes")
	defer res.Body.Close()

	workstations, listing := [][]Workstation{}, true
	body := bufio.NewScanner(res.Body)
	for body.Scan() {
		line := body.Text()

		// Skip any bullshit before the item listing
		if listing {
			if !strings.Contains(line, "<li class=\"toclevel-1 tocsection-") {
				if len(workstations) > 0 {
					listing = false
				}
				continue
			}
			// Parse the needed workstations for the recipes
			if listing {
				ws := []Workstation{}
				for _, match := range regexp.MustCompile("([A-Za-z '-]+)</span>").FindAllStringSubmatch(line, -1) {
					name := strings.TrimSpace(match[1])
					if item, ok := items[name]; ok {
						ws = append(ws, Workstation{Item: item})
					} else {
						ws = append(ws, Workstation{Other: name})
					}
					workstations = append(workstations, ws)
				}
			}
			continue
		}
		// Not listing, skip any bullshit before the workstation details
		if !strings.Contains(line, "<a href=\"/Recipes/") {
			continue
		}
		// Parse the recipe listing page URL and pull in all recipes from there
		url := strings.TrimSpace(regexp.MustCompile("<a href=\"([^\"]+)\"").FindStringSubmatch(line)[1])
		recipes = append(recipes, listWorkstationRecipes(items, workstations[0], url)...)
		workstations = workstations[1:]
	}
	// Assign the IDs to the recipes
	for i := 0; i < len(recipes); i++ {
		recipes[i].Id = i + 1
	}
	return recipes
}

// listWorkstationRecipes retrieves all the recipies produced at a specific workspace,
// possibly grouping multiple recipes in one batch.
func listWorkstationRecipes(items map[string]int, workstations []Workstation, url string) []Recipe {
	recipes := []Recipe{}

	// Retrieve all the workstations to iterate the recipes for
	log15.Debug("parsing workstation recipes", "page", url)
	res, _ := http.Get("http://terraria.gamepedia.com" + url)
	defer res.Body.Close()

	raw, _ := ioutil.ReadAll(res.Body)
	raw = bytes.Replace(raw, []byte("</tr><tr>"), []byte("</tr>\n<tr>"), -1) // Split combined lines

	// Create a scanner that can skip unintersting lines
	body := bufio.NewScanner(bytes.NewReader(raw))
	scan := func() {
		body.Scan()
		if !strings.Contains(body.Text(), "title") {
			body.Scan()
		}
	}
	// Iterate the recipes
	for {
		// Skip any bullshit before the item listing
		line := body.Text()
		if !strings.Contains(line, "style=\"text-align:center;width:1%\">") {
			if body.Scan() {
				continue
			}
			break
		}
		recipe := Recipe{
			Workstations: workstations,
		}
		// Strip out the item name from the next line (each item is in two lines)
		scan()

		item := strings.TrimSpace(regexp.MustCompile("title=\"([^\"]+)\"").FindStringSubmatch(body.Text())[1])
		recipe.Product = Ingredient{
			Item:  items[item],
			Count: 1,
		}
		if match := regexp.MustCompile("\\(([0-9]+)\\)").FindStringSubmatch(body.Text()); len(match) > 0 {
			recipe.Product.Count, _ = strconv.Atoi(match[1])
		}
		components, _ := strconv.Atoi(strings.TrimSpace(regexp.MustCompile("rowspan=\"([0-9]+)\"").FindStringSubmatch(body.Text())[1]))

		// Strip out ingredients from the next lines (each item is in two lines)
		for i := 0; i < components; i++ {
			scan()
			scan()

			name, count := strings.TrimSpace(regexp.MustCompile("title=\"([^\"]+)\"").FindStringSubmatch(body.Text())[1]), 1
			if match := regexp.MustCompile("\\(([0-9]+)\\)").FindStringSubmatch(body.Text()); len(match) > 0 {
				count, _ = strconv.Atoi(match[1])
			}
			recipe.Ingredients = append(recipe.Ingredients, Ingredient{Item: items[name], Count: count})
		}
		recipes = append(recipes, recipe)
	}
	return recipes
}
