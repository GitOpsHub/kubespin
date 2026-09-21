// @ts-check
import {themes as prismThemes} from 'prism-react-renderer';

/** @type {import('@docusaurus/types').Config} */
const config = {
  title: 'kubespin',
  tagline: 'Provision and manage Kubernetes clusters across EKS, GKE, and AKS',
  favicon: 'img/favicon.ico',

  future: {
    v4: true,
  },

  url: 'https://gitopshub.github.io',
  baseUrl: '/kubespin/',

  organizationName: 'GitOpsHub',
  projectName: 'kubespin',

  onBrokenLinks: 'throw',
  onBrokenAnchors: 'throw',

  i18n: {
    defaultLocale: 'en',
    locales: ['en'],
  },

  presets: [
    [
      'classic',
      /** @type {import('@docusaurus/preset-classic').Options} */
      ({
        docs: {
          path: '../docs',
          routeBasePath: '/',
          sidebarPath: './sidebars.js',
          editUrl: 'https://github.com/GitOpsHub/kubespin/edit/main/docs/',
        },
        blog: false,
        theme: {
          customCss: './src/css/custom.css',
        },
      }),
    ],
  ],

  themeConfig:
    /** @type {import('@docusaurus/preset-classic').ThemeConfig} */
    ({
      colorMode: {
        respectPrefersColorScheme: true,
      },
      navbar: {
        title: 'kubespin',
        logo: {
          alt: 'kubespin logo',
          src: 'img/logo.svg',
        },
        items: [
          {
            type: 'docSidebar',
            sidebarId: 'docsSidebar',
            position: 'left',
            label: 'Docs',
          },
          {
            href: 'https://github.com/GitOpsHub/kubespin',
            label: 'GitHub',
            position: 'right',
          },
        ],
      },
      footer: {
        style: 'dark',
        links: [
          {
            title: 'Docs',
            items: [
              {label: 'Home', to: '/'},
              {label: 'CLI reference', to: '/cli/kubespin'},
              {label: 'Code reference', to: '/reference/'},
            ],
          },
          {
            title: 'More',
            items: [
              {
                label: 'GitHub',
                href: 'https://github.com/GitOpsHub/kubespin',
              },
            ],
          },
        ],
        copyright:
          'Outbound-only by design — nothing ever reaches inbound into a cluster.',
      },
      prism: {
        theme: prismThemes.github,
        darkTheme: prismThemes.dracula,
      },
    }),
};

export default config;
