package java

import (
	"encoding/json"
	"github.com/jfrog/gofrog/datastructures"
	"github.com/jfrog/jfrog-cli-core/v2/utils/config"
	"github.com/jfrog/jfrog-cli-core/v2/utils/coreutils"
	xrayutils "github.com/jfrog/jfrog-cli-core/v2/xray/utils"
	xrayUtils "github.com/jfrog/jfrog-client-go/xray/services/utils"
	"os"
	"strconv"
	"strings"
	"time"

	buildinfo "github.com/jfrog/build-info-go/entities"

	artifactoryUtils "github.com/jfrog/jfrog-cli-core/v2/artifactory/utils"
	"github.com/jfrog/jfrog-client-go/utils/errorutils"
)

const (
	GavPackageTypeIdentifier = "gav://"
)

type DependencyTreeParams struct {
	Tool             coreutils.Technology
	InsecureTls      bool
	IgnoreConfigFile bool
	ExcludeTestDeps  bool
	UseWrapper       bool
	Server           *config.ServerDetails
	DepsRepo         string
}

func createBuildConfiguration(buildName string) (*artifactoryUtils.BuildConfiguration, func() error) {
	buildConfiguration := artifactoryUtils.NewBuildConfiguration(buildName, strconv.FormatInt(time.Now().Unix(), 10), "", "")
	return buildConfiguration, func() error {
		buildNumber, err := buildConfiguration.GetBuildNumber()
		if err != nil {
			return err
		}
		return artifactoryUtils.RemoveBuildDir(buildName, buildNumber, buildConfiguration.GetProject())
	}
}

// Create a dependency tree for each one of the modules in the build.
// buildName - audit-mvn or audit-gradle
func createGavDependencyTree(buildConfig *artifactoryUtils.BuildConfiguration) ([]*xrayUtils.GraphNode, []string, error) {
	buildName, err := buildConfig.GetBuildName()
	if err != nil {
		return nil, nil, err
	}
	buildNumber, err := buildConfig.GetBuildNumber()
	if err != nil {
		return nil, nil, err
	}
	generatedBuildsInfos, err := artifactoryUtils.GetGeneratedBuildsInfo(buildName, buildNumber, buildConfig.GetProject())
	if err != nil {
		return nil, nil, err
	}
	if len(generatedBuildsInfos) == 0 {
		return nil, nil, errorutils.CheckErrorf("Couldn't find build " + buildName + "/" + buildNumber)
	}
	var modules []*xrayUtils.GraphNode
	uniqueDepsSet := datastructures.MakeSet[string]()
	for _, module := range generatedBuildsInfos[0].Modules {
		modules = append(modules, addModuleTree(module, uniqueDepsSet))
	}

	return modules, uniqueDepsSet.ToSlice(), nil
}

func addModuleTree(module buildinfo.Module, uniqueDepsSet *datastructures.Set[string]) *xrayUtils.GraphNode {
	moduleTree := &xrayUtils.GraphNode{
		Id: GavPackageTypeIdentifier + module.Id,
	}
	uniqueDepsSet.Add(moduleTree.Id)
	directDependencies := make(map[string]buildinfo.Dependency)
	parentToChildren := newDependencyMultimap()
	for index, dependency := range module.Dependencies {
		requestedBy := dependency.RequestedBy
		if isDirectDependency(module.Id, requestedBy) {
			// If no parents at all or the direct parent is the module, assume dependency is a direct
			directDependencies[dependency.Id] = dependency
			continue
		}

		for _, parent := range requestedBy {
			// we use '&module.Dependencies[index]' to avoid reusing the &dependency pointer
			parentToChildren.putChild(GavPackageTypeIdentifier+parent[0], &module.Dependencies[index])
		}
	}

	for _, directDependency := range directDependencies {
		populateTransitiveDependencies(moduleTree, directDependency.Id, parentToChildren, []string{}, uniqueDepsSet)
	}
	return moduleTree
}

func isDirectDependency(moduleId string, requestedBy [][]string) bool {
	if len(requestedBy) == 0 || len(requestedBy[0]) == 0 {
		// If no parents at all, assume dependency is direct
		return true
	}
	for _, directParent := range requestedBy {
		if directParent[0] == moduleId {
			return true
		}
	}

	return false
}

func populateTransitiveDependencies(parent *xrayUtils.GraphNode, dependencyId string, parentToChildren *dependencyMultimap, idsAdded []string, uniqueDepsSet *datastructures.Set[string]) {
	if hasLoop(idsAdded, dependencyId) {
		return
	}
	idsAdded = append(idsAdded, dependencyId)
	node := &xrayUtils.GraphNode{
		Id:    GavPackageTypeIdentifier + dependencyId,
		Nodes: []*xrayUtils.GraphNode{},
	}
	uniqueDepsSet.Add(node.Id)
	parent.Nodes = append(parent.Nodes, node)
	for _, child := range parentToChildren.getChildren(node.Id) {
		populateTransitiveDependencies(node, child.Id, parentToChildren, idsAdded, uniqueDepsSet)
	}
}

func hasLoop(idsAdded []string, idToAdd string) bool {
	for _, id := range idsAdded {
		if id == idToAdd {
			return true
		}
	}
	return false
}

func BuildDependencyTree(params xrayutils.AuditParams, tech coreutils.Technology) ([]*xrayUtils.GraphNode, []string, error) {
	serverDetails, err := params.ServerDetails()
	if err != nil {
		return nil, nil, err
	}
	dependencyTreeParams := &DependencyTreeParams{
		Tool:             tech,
		InsecureTls:      params.InsecureTls(),
		IgnoreConfigFile: params.IgnoreConfigFile(),
		ExcludeTestDeps:  params.ExcludeTestDependencies(),
		UseWrapper:       params.UseWrapper(),
		Server:           serverDetails,
		DepsRepo:         params.DepsRepo(),
	}
	if tech == coreutils.Maven {
		return buildMvnDependencyTree(dependencyTreeParams)
	}
	return buildGradleDependencyTree(dependencyTreeParams)
}

type dependencyMultimap struct {
	multimap map[string]map[string]*buildinfo.Dependency
}

func newDependencyMultimap() *dependencyMultimap {
	dependencyMultimap := new(dependencyMultimap)
	dependencyMultimap.multimap = make(map[string]map[string]*buildinfo.Dependency)
	return dependencyMultimap
}

func (dm *dependencyMultimap) putChild(parent string, child *buildinfo.Dependency) {
	if dm.multimap[parent] == nil {
		dm.multimap[parent] = make(map[string]*buildinfo.Dependency)
	}
	dm.multimap[parent][child.Id] = child
}

func (dm *dependencyMultimap) getChildren(parent string) map[string]*buildinfo.Dependency {
	return dm.multimap[parent]
}

// The structure of a dependency tree of a module in a Gradle/Maven project, as created by the gradle-dep-tree and maven-dep-tree plugins.
type moduleDepTree struct {
	Root  string                 `json:"root"`
	Nodes map[string]depTreeNode `json:"nodes"`
}

type depTreeNode struct {
	Children []string `json:"children"`
}

// getGraphFromDepTree reads the output files of the gradle-dep-tree and maven-dep-tree plugins and returns them as a slice of GraphNodes.
// It takes the output of the plugin's run (which is a byte representation of a list of paths of the output files, separated by newlines) as input.
func getGraphFromDepTree(depTreeOutput []byte) (depsGraph []*xrayUtils.GraphNode, uniqueDeps []string, err error) {
	modules, err := parseDepTreeFiles(depTreeOutput)
	if err != nil {
		return
	}
	uniqueDepsSet := datastructures.MakeSet[string]()
	for _, moduleTree := range modules {
		directDependency := &xrayUtils.GraphNode{
			Id:    GavPackageTypeIdentifier + moduleTree.Root,
			Nodes: []*xrayUtils.GraphNode{},
		}
		populateDependencyTree(directDependency, moduleTree.Root, moduleTree, uniqueDepsSet)
		depsGraph = append(depsGraph, directDependency)
	}
	uniqueDeps = uniqueDepsSet.ToSlice()
	return
}

func populateDependencyTree(currNode *xrayUtils.GraphNode, currNodeId string, moduleTree *moduleDepTree, uniqueDepsSet *datastructures.Set[string]) {
	if currNode.NodeHasLoop() {
		return
	}
	for _, childId := range moduleTree.Nodes[currNodeId].Children {
		childGav := GavPackageTypeIdentifier + childId
		childNode := &xrayUtils.GraphNode{
			Id:     childGav,
			Nodes:  []*xrayUtils.GraphNode{},
			Parent: currNode,
		}
		uniqueDepsSet.Add(childGav)
		populateDependencyTree(childNode, childId, moduleTree, uniqueDepsSet)
		currNode.Nodes = append(currNode.Nodes, childNode)
	}
}

func parseDepTreeFiles(jsonFilePaths []byte) ([]*moduleDepTree, error) {
	outputFilePaths := strings.Split(strings.TrimSpace(string(jsonFilePaths)), "\n")
	var modules []*moduleDepTree
	for _, path := range outputFilePaths {
		results, err := parseDepTreeFile(path)
		if err != nil {
			return nil, err
		}
		modules = append(modules, results)
	}
	return modules, nil
}

func parseDepTreeFile(path string) (results *moduleDepTree, err error) {
	depTreeJson, err := os.ReadFile(strings.TrimSpace(path))
	if errorutils.CheckError(err) != nil {
		return
	}
	results = &moduleDepTree{}
	err = errorutils.CheckError(json.Unmarshal(depTreeJson, &results))
	return
}
