import * as core from '@actions/core'
import * as exec from '@actions/exec'
import * as github from '@actions/github'
import {DefaultArtifactClient} from '@actions/artifact'
import * as tc from '@actions/tool-cache'
import fs from 'fs'
import os from 'os'
import path from 'path'
import {fileURLToPath} from 'url'

const commentMarker = '<!-- fmp-comment-marker -->'
const moduleDir = path.dirname(fileURLToPath(import.meta.url))

async function run() {
  let exitCode = 0
  let execError = null

  try {
    const binaryPath = await resolveBinaryPath()
    maskAITokens()

    await core.group('Run gitops-preview-toolkit github-action', async () => {
      core.info(`Using preview binary: ${binaryPath}`)
      try {
        exitCode = await exec.exec(binaryPath, ['github-action'], {
          ignoreReturnCode: true
        })
      } catch (err) {
        execError = err
      }
	    })

	    const report = readReport()
	    let reportUrl = null
	    if (booleanInput('html-report', false)) {
	      reportUrl = await maybeUploadHTMLReport(report)
	    }
	    await maybeSyncComment(report, reportUrl)
	    try {
	      await maybeSyncLabels(report)
	    } catch (err) {
	      core.warning(`PR label sync failed: ${err instanceof Error ? err.message : String(err)}`)
	    }

    if (execError) {
      throw execError
    }
    if (exitCode !== 0) {
      throw new Error(`gitops-preview-toolkit github-action failed with exit code ${exitCode}`)
    }
  } catch (err) {
    core.setFailed(err instanceof Error ? err.message : String(err))
  }
}

function maskAITokens() {
  for (const name of ['OPENAI_API_KEY', 'ANTHROPIC_API_KEY', 'ZAI_API_KEY', 'MINIMAX_API_KEY', 'OPENROUTER_API_KEY']) {
    const value = (process.env[name] || '').trim()
    if (value !== '') {
      core.setSecret(value)
    }
  }
}

async function maybeUploadHTMLReport(report) {
  if (!report || !report.html_report_file) {
    core.warning('Skipping HTML report upload because fmp did not write html_report_file')
    return null
  }
  if (!fs.existsSync(report.html_report_file)) {
    core.warning(`Skipping HTML report upload because ${report.html_report_file} does not exist`)
    return null
  }

  const htmlContent = fs.readFileSync(report.html_report_file, 'utf8')
  const {owner, repo} = github.context.repo
  const runId = github.context.runId
  const pagesPath = stringInput('html-report-pages-path', 'fmp-reports')
  let reportUrl = null

  if (booleanInput('html-report-pages', false)) {
    reportUrl = await deployToGitHubPages(htmlContent, `${pagesPath}/${runId}`, 'index.html')
  }

  if (!reportUrl) {
    const artifactName = stringInput('html-report-name', 'flux-manifest-preview-report')
    const retentionDays = integerInput('html-report-retention-days', 7)
    const rootDirectory = path.dirname(report.html_report_file)
    const client = new DefaultArtifactClient()
    const result = await client.uploadArtifact(artifactName, [report.html_report_file], rootDirectory, {retentionDays, skipArchive: true})
    reportUrl = `https://github.com/${owner}/${repo}/actions/runs/${runId}/artifacts/${result.id}`
    core.setOutput('html-report-artifact', artifactName)
  }

  core.setOutput('html-report-url', reportUrl)
  return reportUrl
}

async function deployToGitHubPages(htmlContent, dirPath, fileName) {
  const token = stringInput('github-token', '')
  if (token === '') {
    core.warning('Skipping GitHub Pages deployment because github-token is empty')
    return null
  }

  const octokit = github.getOctokit(token)
  const {owner, repo} = github.context.repo
  const branch = 'gh-pages'

  let baseTree
  try {
    const {data: ref} = await octokit.rest.git.getRef({owner, repo, ref: `heads/${branch}`})
    const {data: commit} = await octokit.rest.git.getCommit({owner, repo, commit_sha: ref.object.sha})
    baseTree = commit.tree.sha
  } catch {
    const {data: masterRef} = await octokit.rest.git.getRef({owner, repo, ref: 'heads/main'})
    const emptyTree = await octokit.rest.git.createTree({owner, repo, tree: [], base_tree: masterRef.object.sha})
    const {data: initCommit} = await octokit.rest.git.createCommit({
      owner, repo,
      message: 'initialize gh-pages',
      tree: emptyTree.sha,
      parents: []
    })
    await octokit.rest.git.createRef({owner, repo, ref: `refs/heads/${branch}`, sha: initCommit.sha})
    baseTree = null
  }

  const blob = await octokit.rest.git.createBlob({
    owner, repo,
    content: htmlContent,
    encoding: 'utf-8'
  })

  const treeItems = [{
    path: `${dirPath}/${fileName}`,
    mode: '100644',
    type: 'blob',
    sha: blob.data.sha
  }]

  const tree = await octokit.rest.git.createTree({
    owner, repo,
    tree: treeItems,
    base_tree: baseTree || undefined
  })

  const parentRef = await octokit.rest.git.getRef({owner, repo, ref: `heads/${branch}`})
  const newCommit = await octokit.rest.git.createCommit({
    owner, repo,
    message: `deploy flux manifest preview report`,
    tree: tree.data.sha,
    parents: [parentRef.data.object.sha]
  })

  await octokit.rest.git.updateRef({
    owner, repo,
    ref: `heads/${branch}`,
    sha: newCommit.data.sha
  })

  return `https://${owner}.github.io/${repo}/${dirPath}/${fileName}`
}

async function resolveBinaryPath() {
  const customBinary = core.getInput('binary').trim()
  if (customBinary !== '') {
    return ensureBinary(resolveInputPath(customBinary))
  }

  const version = resolveActionVersion()
  if (!/^v.+/.test(version)) {
    throw new Error('This action does not build the toolkit from source. For non-release refs, provide `with: binary:` and install sibling plugins.')
  }

  const toolArch = process.arch
  const cachedDir = tc.find('gitops-preview-toolkit', version, toolArch)
  if (cachedDir !== '') {
    return ensureToolkit(cachedDir)
  }

  return core.group(`Download gitops-preview-toolkit ${version}`, async () => {
    const archivePath = await tc.downloadTool(releaseURL(version))
    const extractedDir = await tc.extractTar(archivePath)
    ensureToolkit(extractedDir)
    const installedDir = await tc.cacheDir(extractedDir, 'gitops-preview-toolkit', version, toolArch)
    return ensureToolkit(installedDir)
  })
}

function resolveActionVersion() {
  const envOverride = (process.env.FMP_ACTION_REF || '').trim()
  if (envOverride !== '') {
    return envOverride
  }
  return path.basename(path.resolve(moduleDir, '..'))
}

function releaseURL(version) {
  const platform = platformName(process.platform)
  const arch = archiveArch(process.arch)
  return `https://github.com/tobiash/gitops-preview-toolkit/releases/download/${version}/gitops-preview-toolkit_${version}_${platform}_${arch}.tar.gz`
}

function resolveInputPath(inputPath) {
  if (path.isAbsolute(inputPath)) {
    return inputPath
  }

  const workspace = process.env.GITHUB_WORKSPACE || process.cwd()
  return path.resolve(workspace, inputPath)
}

function ensureBinary(binaryPath) {
  if (!fs.existsSync(binaryPath)) {
    throw new Error(`Preview executable not found at ${binaryPath}`)
  }

  if (process.platform !== 'win32') {
    fs.chmodSync(binaryPath, 0o755)
  }

  return binaryPath
}

function readReport() {
  const reportPath = path.join(process.env.RUNNER_TEMP || os.tmpdir(), 'fmp-report.json')
  if (!fs.existsSync(reportPath)) {
    return null
  }

  return JSON.parse(fs.readFileSync(reportPath, 'utf8'))
}

async function maybeSyncComment(report, reportUrl) {
  if (!booleanInput('comment', false)) {
    return
  }
  if (github.context.eventName !== 'pull_request') {
    core.info('Skipping PR comment sync outside pull_request events')
    return
  }
  if (!report) {
    core.warning('Skipping PR comment sync because fmp-report.json was not written')
    return
  }

  const token = stringInput('github-token', '')
  if (token === '') {
    core.warning('Skipping PR comment sync because github-token is empty')
    return
  }
  core.setSecret(token)

  const octokit = github.getOctokit(token)
  const {owner, repo} = github.context.repo
  const issueNumber = github.context.issue.number
  if (!issueNumber) {
    core.warning('Skipping PR comment sync because pull request context is missing issue number')
    return
  }

  const comments = await octokit.paginate(octokit.rest.issues.listComments, {
    owner,
    repo,
    issue_number: issueNumber,
    per_page: 100
  })
  const existingComment = comments.find(comment => typeof comment.body === 'string' && comment.body.includes(commentMarker))

  if (report.status === 'clean' && stringInput('comment-mode', 'changes') === 'changes') {
    if (existingComment) {
      await octokit.rest.issues.deleteComment({
        owner,
        repo,
        comment_id: existingComment.id
      })
    }
    return
  }

  const commentPath = report.comment_file || path.join(process.env.RUNNER_TEMP || os.tmpdir(), 'fmp-comment.md')
  if (!fs.existsSync(commentPath)) {
    return
  }

  const body = fs.readFileSync(commentPath, 'utf8')
  let finalBody = body

  if (reportUrl) {
    const isPages = reportUrl.includes('github.io')
    const label = isPages ? 'View interactive HTML report' : 'Download interactive HTML report'
    const reportLink = `\n📊 **[${label}](${reportUrl})**\n\n`
    finalBody = body.replace('<!-- fmp-comment-marker -->', reportLink + '<!-- fmp-comment-marker -->')
  }

  if (existingComment) {
    await octokit.rest.issues.updateComment({
      owner,
      repo,
      comment_id: existingComment.id,
      body: finalBody
    })
    return
  }

  await octokit.rest.issues.createComment({
    owner,
    repo,
    issue_number: issueNumber,
    body: finalBody
  })
}

async function maybeSyncLabels(report) {
  if (github.context.eventName !== 'pull_request') {
    return
  }
  if (!report || !Array.isArray(report.labels) || report.labels.length === 0) {
    return
  }

  const token = stringInput('github-token', '')
  if (token === '') {
    core.warning('Skipping PR label sync because github-token is empty')
    return
  }

  const octokit = github.getOctokit(token)
  const {owner, repo} = github.context.repo
  const issueNumber = github.context.issue.number
  if (!issueNumber) {
    core.warning('Skipping PR label sync because pull request context is missing issue number')
    return
  }

  await octokit.rest.issues.addLabels({
    owner,
    repo,
    issue_number: issueNumber,
    labels: report.labels
  })
}

function platformName(platform) {
  switch (platform) {
    case 'linux':
      return 'linux'
    case 'darwin':
      return 'darwin'
    default:
      throw new Error(`unsupported runner platform: ${platform}`)
  }
}

function archiveArch(arch) {
  switch (arch) {
    case 'x64':
      return 'amd64'
    case 'arm64':
      return 'arm64'
    default:
      throw new Error(`unsupported runner architecture: ${arch}`)
  }
}

function binaryName() {
  if (process.platform === 'win32') {
    return 'gitops-preview.exe'
  }
  return 'gitops-preview'
}

function ensureToolkit(directory) {
  for (const name of ['gitops-preview-flux', 'gitops-preview-crossplane', 'fmp']) {
    ensureBinary(path.join(directory, name))
  }
  // Agent/MCP services use PATH; ordinary commands also discover host siblings.
  core.addPath(directory)
  return ensureBinary(path.join(directory, binaryName()))
}

function stringInput(name, defaultValue) {
  const value = core.getInput(name).trim()
  if (value === '') {
    return defaultValue
  }
  return value
}

function booleanInput(name, defaultValue) {
  const value = stringInput(name, '')
  if (value === '') {
    return defaultValue
  }
  return value.toLowerCase() === 'true'
}

function integerInput(name, defaultValue) {
  const value = stringInput(name, '')
  if (value === '') {
    return defaultValue
  }
  const parsed = Number.parseInt(value, 10)
  return Number.isFinite(parsed) && parsed > 0 ? parsed : defaultValue
}

void run()
