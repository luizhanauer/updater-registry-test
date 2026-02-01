package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"regexp"
	"strings"
	"time"
)

// ==========================================
// ESTRUTURAS DE DADOS
// ==========================================

type SourceApp struct {
	ID          string            `json:"id"`
	Name        string            `json:"name"`
	Description string            `json:"description"`
	IconURL     string            `json:"icon_url"`
	PackageName string            `json:"package_name"`
	InstallType string            `json:"install_type"`
	Strategy    string            `json:"strategy"` // "github_release", "direct_url_head", "direct_static"
	Config      map[string]string `json:"config"`
}

type CatalogApp struct {
	// Campos herdados (Metadata)
	ID          string `json:"id"`
	Name        string `json:"name"`
	Description string `json:"description"`
	IconURL     string `json:"icon_url"`
	PackageName string `json:"package_name"`
	InstallType string `json:"install_type"`

	// Campos dinâmicos (Atualização)
	Version     string `json:"latest_version"`
	DownloadURL string `json:"download_url"`
	Checksum    string `json:"checksum"` // SHA256
	Size        int64  `json:"size"`     // Tamanho em bytes
}

type Catalog struct {
	LastUpdated time.Time             `json:"last_updated"`
	Apps        map[string]CatalogApp `json:"apps"`
}

// Estrutura auxiliar para API do GitHub
type GithubRelease struct {
	TagName string `json:"tag_name"`
	Assets  []struct {
		Name               string `json:"name"`
		BrowserDownloadURL string `json:"browser_download_url"`
		Size               int64  `json:"size"`
	} `json:"assets"`
}

// ==========================================
// MAIN
// ==========================================

func main() {
	log.Println(">>> Iniciando Gerador de Catálogo...")

	// 1. Carregar Configuração e Catálogo Antigo
	sources := loadSources("apps.source.json")
	oldCatalog := loadCatalog("catalog.json") // Se não existir, retorna vazio

	newCatalog := Catalog{
		LastUpdated: time.Now(),
		Apps:        make(map[string]CatalogApp),
	}

	changesCount := 0

	// 2. Processar cada App
	for _, src := range sources {
		log.Printf("------------------------------------------------")
		log.Printf("Processando: %s (%s)", src.Name, src.Strategy)

		// Passo A: Identificar versão online e URL (sem baixar se possível)
		onlineVer, onlineURL, onlineSize, err := checkStrategy(src)
		if err != nil {
			log.Printf(" [ERRO] Falha ao checar %s: %v. Mantendo versão antiga.", src.ID, err)
			if old, ok := oldCatalog.Apps[src.ID]; ok {
				newCatalog.Apps[src.ID] = old
			}
			continue
		}

		// Passo B: Verificar se precisa atualizar
		// Se for "direct_static", a versão é sempre "latest" ou data, então forçamos a checagem de hash depois
		forceCheck := src.Strategy == "direct_static"
		
		oldApp, exists := oldCatalog.Apps[src.ID]
		
		if exists && !forceCheck && oldApp.Version == onlineVer {
			log.Printf(" [SKIP] Versão inalterada (%s). Mantendo cache.", onlineVer)
			newCatalog.Apps[src.ID] = oldApp
			continue
		}

		// Passo C: Baixar e Calcular Hash
		log.Printf(" [UPDATE] Nova versão detectada ou check forçado (%s -> %s). Baixando...", oldApp.Version, onlineVer)

		checksum, downloadedSize, err := downloadAndHash(onlineURL)
		if err != nil {
			log.Printf(" [ERRO] Falha no download de %s: %v", src.ID, err)
			// Mantém o antigo em caso de falha no download
			if exists { newCatalog.Apps[src.ID] = oldApp }
			continue
		}

		// Para estratégia estática (Chrome), se o hash for igual, não atualizamos a data
		if forceCheck && exists && oldApp.Checksum == checksum {
			log.Printf(" [SKIP] Hash do arquivo estático não mudou. Mantendo.",)
			newCatalog.Apps[src.ID] = oldApp
			continue
		}

		// Se o tamanho veio zerado da estratégia (ex: alguns servers não mandam Content-Length no HEAD),
		// usamos o tamanho real do arquivo baixado.
		finalSize := onlineSize
		if finalSize == 0 {
			finalSize = downloadedSize
		}

		// Monta o novo objeto
		newApp := CatalogApp{
			ID:          src.ID,
			Name:        src.Name,
			Description: src.Description,
			IconURL:     src.IconURL,
			PackageName: src.PackageName,
			InstallType: src.InstallType,
			Version:     onlineVer,
			DownloadURL: onlineURL,
			Checksum:    checksum,
			Size:        finalSize,
		}

		newCatalog.Apps[src.ID] = newApp
		changesCount++
		log.Printf(" [SUCESSO] Atualizado para versão %s (Size: %d bytes)", onlineVer, finalSize)
	}

	// 3. Salvar
	if changesCount > 0 || len(oldCatalog.Apps) == 0 {
		saveCatalog("catalog.json", newCatalog)
		log.Printf(">>> Catálogo salvo com %d alterações.", changesCount)
	} else {
		log.Println(">>> Nenhuma alteração necessária.")
	}
}

// ==========================================
// ESTRATÉGIAS
// ==========================================

func checkStrategy(src SourceApp) (version string, url string, size int64, err error) {
	switch src.Strategy {
	case "github_release":
		return checkGithub(src.Config["repo"], src.Config["asset_filter"])
	case "direct_url_head":
		return checkDirectHead(src.Config["url"], src.Config["regex"])
	case "direct_static":
		// Para links estáticos (ex: Chrome), a versão é a data de hoje
		// O download real vai confirmar se o hash mudou
		return time.Now().Format("2006.01.02"), src.Config["url"], 0, nil
	default:
		return "", "", 0, fmt.Errorf("estratégia desconhecida: %s", src.Strategy)
	}
}

// Estratégia 1: GitHub API
func checkGithub(repo, assetFilter string) (string, string, int64, error) {
	url := fmt.Sprintf("https://api.github.com/repos/%s/releases/latest", repo)
	req, _ := http.NewRequest("GET", url, nil)
	
	// Token é obrigatório no Actions para não tomar rate limit
	if token := os.Getenv("GITHUB_TOKEN"); token != "" {
		req.Header.Set("Authorization", "token "+token)
	}

	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil { return "", "", 0, err }
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		return "", "", 0, fmt.Errorf("github status: %d", resp.StatusCode)
	}

	var rel GithubRelease
	if err := json.NewDecoder(resp.Body).Decode(&rel); err != nil { return "", "", 0, err }

	version := strings.TrimPrefix(rel.TagName, "v")
	
	for _, asset := range rel.Assets {
		if strings.Contains(strings.ToLower(asset.Name), assetFilter) {
			return version, asset.BrowserDownloadURL, asset.Size, nil
		}
	}

	return "", "", 0, fmt.Errorf("asset '%s' não encontrado na release", assetFilter)
}

// Estratégia 2: HEAD Request com Redirect + Regex
func checkDirectHead(startURL, versionRegex string) (string, string, int64, error) {
	client := &http.Client{Timeout: 10 * time.Second}

	// HEAD segue redirects por padrão no Go
	req, _ := http.NewRequest("HEAD", startURL, nil)
	req.Header.Set("User-Agent", "Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36")

	resp, err := client.Do(req)
	if err != nil { return "", "", 0, err }
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		return "", "", 0, fmt.Errorf("status invalido: %d", resp.StatusCode)
	}

	finalURL := resp.Request.URL.String()
	size := resp.ContentLength // Tenta pegar o tamanho do header

	// Extrai versão da URL final
	re := regexp.MustCompile(versionRegex)
	matches := re.FindStringSubmatch(finalURL)

	if len(matches) < 2 {
		return "", "", 0, fmt.Errorf("regex falhou na url: %s", finalURL)
	}

	return matches[1], finalURL, size, nil
}

// ==========================================
// UTILITÁRIOS (IO/HASH)
// ==========================================

// downloadAndHash baixa o arquivo para calcular SHA256 e tamanho real
func downloadAndHash(url string) (string, int64, error) {
	resp, err := http.Get(url)
	if err != nil { return "", 0, err }
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		return "", 0, fmt.Errorf("http status %d", resp.StatusCode)
	}

	// Criamos um hasher
	hasher := sha256.New()
	
	// Copiamos o stream do download para o hasher
	// O io.Copy retorna o número de bytes copiados (tamanho do arquivo)
	size, err := io.Copy(hasher, resp.Body)
	if err != nil { return "", 0, err }

	checksum := hex.EncodeToString(hasher.Sum(nil))
	return checksum, size, nil
}

func loadSources(path string) []SourceApp {
	file, err := os.ReadFile(path)
	if err != nil { log.Fatal(err) }
	var sources []SourceApp
	json.Unmarshal(file, &sources)
	return sources
}

func loadCatalog(path string) Catalog {
	file, err := os.ReadFile(path)
	if err != nil { return Catalog{Apps: make(map[string]CatalogApp)} }
	var catalog Catalog
	json.Unmarshal(file, &catalog.Apps) // Note: ajustado para struct simplificada ou map direto
	// Se o JSON salvar direto o map "apps", ajuste aqui. 
	// Para compatibilidade com o formato proposto anteriormente:
	var temp struct {
		Apps map[string]CatalogApp `json:"apps"`
	}
	if json.Unmarshal(file, &temp) == nil && temp.Apps != nil {
		return Catalog{Apps: temp.Apps}
	}
	// Fallback se o arquivo for apenas o map direto
	json.Unmarshal(file, &catalog.Apps)
	return catalog
}

func saveCatalog(path string, catalog Catalog) {
	// Salvamos o objeto completo com timestamp
	data, _ := json.MarshalIndent(catalog, "", "  ")
	os.WriteFile(path, data, 0644)
}