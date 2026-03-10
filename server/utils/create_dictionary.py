import requests
import json
import time
import os

# Define the API endpoint
API_URL = "https://api.dictionaryapi.dev/api/v2/entries/en/"

# URL of the words file
WORD_URL = "https://raw.githubusercontent.com/dolph/dictionary/refs/heads/master/popular.txt"

# Local file paths
local_file = "./words.txt"
output_file = './word_definitions.json'

# Download the file if it doesn't exist locally
if not os.path.exists(local_file):
    try:
        print(f"Downloading words.txt from {WORD_URL}...")
        response = requests.get(WORD_URL)
        response.raise_for_status()
        with open(local_file, "w") as f:
            f.write(response.text)
        print(f"Downloaded and saved as {local_file}.")
    except requests.exceptions.RequestException as e:
        print(f"Failed to download the file: {e}")
        exit(1)

# Load existing definitions if available
if os.path.exists(output_file):
    with open(output_file, 'r') as json_file:
        word_definitions = json.load(json_file)
else:
    word_definitions = {}

# Load the word list
with open(local_file) as f:
    all_words = [line.strip() for line in f.readlines() if line.strip()]

# Skip already-processed words
remaining_words = [word for word in all_words if word not in word_definitions]

# Total words to process
total_words = len(all_words)
remaining_count = len(remaining_words)

print(f"Total words: {total_words}")
print(f"Words already processed: {total_words - remaining_count}")
print(f"Remaining words to process: {remaining_count}")

# Process the remaining words
for idx, word in enumerate(remaining_words, start=1):
    time.sleep(0.1)  # Delay to avoid rate-limiting

    try:
        # Make an API request for the word
        response = requests.get(API_URL + word)
        response.raise_for_status()

        # Parse the API response
        data = response.json()
        if data and isinstance(data, list):
            word_definitions[word] = {}
            for meaning in data[0].get("meanings", []):
                part_of_speech = meaning.get("partOfSpeech", "unknown")
                definitions = meaning.get("definitions", [])
                if definitions:
                    word_definitions[word][part_of_speech] = []
                    for d in definitions:
                        entry = {"definition": d["definition"]}
                        if "example" in d:
                            entry["example"] = d["example"]
                        word_definitions[word][part_of_speech].append(entry)
        else:
            print(f"No definitions found for: {word}")
    except requests.exceptions.RequestException as e:
        print(f"Request failed for {word}: {e}")
    except (KeyError, IndexError, json.JSONDecodeError):
        print(f"Parsing error for {word}")

    # Save progress after each word
    with open(output_file, 'w') as json_file:
        json.dump(word_definitions, json_file, indent=4)

    # Print progress every 100 words
    if idx % 100 == 0 or idx == remaining_count:
        print(f"Progress: {idx}/{remaining_count} words processed ({(idx / remaining_count) * 100:.2f}%)")

print("All definitions saved to word_definitions.json!")
