"""
Generate Portuguese dictionary for WordWeaver.

Word source: FrequencyWords Portuguese frequency list (OpenSubtitles corpus)
  - https://github.com/hermitdave/FrequencyWords
  - Words are ranked by frequency (most common first) from ~62M subtitle tokens
  - We take the top 15,000 words (after filtering for 3-7 letter alphabetic words)
  - This ensures players encounter common, recognizable Portuguese words

Definitions: Portuguese Wiktionary (pt.wiktionary.org, CC-BY-SA)

Usage:
    python create_dictionary_pt.py

Output: dictionary_pt.json (deploy to /data/assets/games/dictionary_pt.json)

License: FrequencyWords (MIT), Wiktionary definitions (CC-BY-SA).
"""

import requests
import json
import time
import os
import re

WIKTIONARY_API = "https://pt.wiktionary.org/w/api.php"
HEADERS = {"User-Agent": "WordWeaverDictBot/1.0 (game dictionary builder)"}

# Portuguese frequency word list from FrequencyWords project (OpenSubtitles corpus).
# File is pre-sorted by frequency (most common words first). Format: "word count" per line.
WORD_LIST_URL = "https://raw.githubusercontent.com/hermitdave/FrequencyWords/master/content/2018/pt/pt_50k.txt"

local_file = "./words_pt.txt"
output_file = "./dictionary_pt.json"

# Download the frequency word list if not present
if not os.path.exists(local_file):
    try:
        print(f"Downloading Portuguese word list from {WORD_LIST_URL}...")
        response = requests.get(WORD_LIST_URL)
        response.raise_for_status()
        words = []
        for line in response.text.strip().split("\n"):
            parts = line.strip().split()
            if parts:
                word = parts[0].lower()
                # Filter: only alphabetic (including accented), 3-7 chars (game board is 7x7)
                if re.match(r'^[a-záàâãéêíóôõúüç]+$', word) and 3 <= len(word) <= 7:
                    words.append(word)

        words = words[:15000]
        with open(local_file, "w", encoding="utf-8") as f:
            f.write("\n".join(words))
        print(f"Saved {len(words)} frequency-ranked words to {local_file}")
        print(f"Top 20 most frequent words: {', '.join(words[:20])}")
    except requests.exceptions.RequestException as e:
        print(f"Failed to download word list: {e}")
        exit(1)

# Load existing definitions if available
if os.path.exists(output_file):
    with open(output_file, 'r', encoding='utf-8') as json_file:
        word_definitions = json.load(json_file)
else:
    word_definitions = {}

# Load the word list
with open(local_file, encoding='utf-8') as f:
    all_words = [line.strip() for line in f.readlines() if line.strip()]

# Only reprocess words that have empty definitions
remaining_words = [
    word for word in all_words
    if word not in word_definitions
    or word_definitions[word] == {"word": [{"definition": ""}]}
]

total_words = len(all_words)
remaining_count = len(remaining_words)

print(f"Total words: {total_words}")
print(f"Words already processed: {total_words - remaining_count}")
print(f"Remaining words to process: {remaining_count}")


def fetch_pt_definition(word):
    """Fetch definition from Portuguese Wiktionary."""
    try:
        params = {
            "action": "parse",
            "page": word,
            "prop": "wikitext",
            "format": "json",
        }
        resp = requests.get(WIKTIONARY_API, params=params, headers=HEADERS, timeout=10)
        if resp.status_code != 200:
            return None

        data = resp.json()
        if "error" in data:
            return None

        wt = data.get("parse", {}).get("wikitext", {}).get("*", "")

        # Find Portuguese section: ={{-pt-}}=
        pt_match = re.search(
            r'=\s*\{\{-pt-\}\}\s*=(.*?)(?=\n=\s*\{\{-\w+-\}\}\s*=|\Z)',
            wt, re.DOTALL,
        )
        if not pt_match:
            return None

        section = pt_match.group(1)
        result = {}

        pos_map = {
            "Substantivo": "substantivo",
            "Verbo": "verbo",
            "Adjetivo": "adjetivo",
            "Advérbio": "advérbio",
            "Pronome": "pronome",
            "Preposição": "preposição",
            "Conjunção": "conjunção",
            "Interjeição": "interjeição",
            "Artigo": "artigo",
            "Numeral": "numeral",
        }
        pos_pattern = r'==\s*(' + '|'.join(re.escape(k) for k in pos_map.keys()) + r')\s*=='
        pos_matches = list(re.finditer(pos_pattern, section))

        for i, match in enumerate(pos_matches):
            pos = match.group(1)
            start = match.end()
            end = pos_matches[i + 1].start() if i + 1 < len(pos_matches) else len(section)
            sec = section[start:end]

            definitions = []
            for line in sec.split("\n"):
                line = line.strip()
                if line.startswith("#") and not line.startswith("#*") and not line.startswith("#:"):
                    defn = re.sub(r'\{\{[^}]*\}\}', '', line[1:])
                    defn = re.sub(r'\[\[([^|\]]*\|)?([^\]]*)\]\]', r'\2', defn)
                    defn = re.sub(r"'{2,}", '', defn)
                    defn = re.sub(r'<[^>]*>', '', defn)
                    defn = defn.strip(" ,;")
                    if defn and len(defn) > 1:
                        definitions.append({"definition": defn})

            if definitions:
                result[pos_map[pos]] = definitions

        return result if result else None

    except Exception:
        return None


for idx, word in enumerate(remaining_words, start=1):
    time.sleep(0.15)

    definition = fetch_pt_definition(word)

    if definition:
        word_definitions[word] = definition
    elif word not in word_definitions:
        word_definitions[word] = {"word": [{"definition": ""}]}

    # Save progress every 100 words
    if idx % 100 == 0 or idx == remaining_count:
        with open(output_file, 'w', encoding='utf-8') as json_file:
            json.dump(word_definitions, json_file, indent=4, ensure_ascii=False)
        print(f"Progress: {idx}/{remaining_count} words processed")

# Final save
with open(output_file, 'w', encoding='utf-8') as json_file:
    json.dump(word_definitions, json_file, indent=4, ensure_ascii=False)

print(f"Done! {len(word_definitions)} Portuguese words saved to {output_file}")
