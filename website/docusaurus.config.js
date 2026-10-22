import {themes as prismThemes} from 'prism-react-renderer';
import repoLinks from './src/remark/repo-links.js';
import examples from './src/remark/examples.js';

const repo = 'https://github.com/llm-d-incubation/llm-d-resiliency-operator';

export default {
  title: 'llm-d Resiliency Operator',
  favicon: 'img/favicon.png',
  url: 'https://llm-d-incubation.github.io',
  // PR previews are built under a subpath of the same site.
  baseUrl: process.env.DOCS_BASE_URL || '/llm-d-resiliency-operator/',
  onBrokenLinks: 'throw',
  markdown: {mermaid: true, format: 'detect'},
  themes: ['@docusaurus/theme-mermaid'],
  presets: [
    [
      'classic',
      {
        docs: {
          path: '../docs',
          routeBasePath: 'docs',
          sidebarPath: './sidebars.js',
          remarkPlugins: [repoLinks],
          beforeDefaultRemarkPlugins: [examples],
        },
        blog: false,
        theme: {customCss: './src/css/custom.css'},
      },
    ],
  ],
  themeConfig: {
    colorMode: {respectPrefersColorScheme: true},
    docs: {sidebar: {hideable: true}},
    navbar: {
      logo: {
        alt: 'llm-d Resiliency Operator',
        src: 'img/llm-d-logo-transparent.png',
        srcDark: 'img/llm-d-logo-transparent-dark.png',
      },
      items: [
        {type: 'docSidebar', sidebarId: 'docs', position: 'left', label: 'Docs'},
        {href: repo, position: 'right', className: 'header-github-link', 'aria-label': 'GitHub'},
      ],
    },
    footer: {
      style: 'dark',
      links: [
        {title: 'Docs', items: [{label: 'Architecture', to: '/docs'}, {label: 'Components', to: '/docs/components'}, {label: 'Failures', to: '/docs/failures'}]},
        {title: 'Community', items: [{label: 'GitHub', href: repo}, {label: 'llm-d', href: 'https://llm-d.ai'}]},
      ],
      copyright: `Copyright © ${new Date().getFullYear()} llm-d Project Authors. Apache-2.0.`,
    },
    prism: {
      theme: prismThemes.oneLight,
      darkTheme: prismThemes.oneDark,
    },
    mermaid: {theme: {light: 'neutral', dark: 'dark'}},
  },
};
